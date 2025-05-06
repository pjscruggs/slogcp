// Copyright 2025 Patrick J. Scruggs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package http

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// mockResponseWriter simulates http.ResponseWriter for testing the wrapper.
type mockResponseWriter struct {
	header      http.Header
	body        *bytes.Buffer
	wroteHeader bool
	statusCode  int
}

func newMockResponseWriter() *mockResponseWriter {
	return &mockResponseWriter{
		header: make(http.Header),
		body:   new(bytes.Buffer),
	}
}

func (m *mockResponseWriter) Header() http.Header {
	return m.header
}

func (m *mockResponseWriter) Write(data []byte) (int, error) {
	if !m.wroteHeader {
		m.WriteHeader(http.StatusOK) // Default if not called
	}
	return m.body.Write(data)
}

func (m *mockResponseWriter) WriteHeader(statusCode int) {
	if m.wroteHeader {
		return // Standard library behavior: ignore subsequent calls
	}
	m.statusCode = statusCode
	m.wroteHeader = true
}

// TestResponseWriter_WriteHeader verifies status code setting and idempotency.
func TestResponseWriter_WriteHeader(t *testing.T) {
	mock := newMockResponseWriter()
	rw := &responseWriter{ResponseWriter: mock}

	// Set a header *before* WriteHeader
	rw.Header().Set("X-Before", "true")

	// First call
	rw.WriteHeader(http.StatusAccepted) // 202
	if rw.statusCode != http.StatusAccepted {
		t.Errorf("First WriteHeader: got statusCode %d, want %d", rw.statusCode, http.StatusAccepted)
	}
	if !rw.wroteHeader {
		t.Error("First WriteHeader: wroteHeader should be true")
	}
	if mock.statusCode != http.StatusAccepted {
		t.Errorf("First WriteHeader: underlying writer got statusCode %d, want %d", mock.statusCode, http.StatusAccepted)
	}
	if !mock.wroteHeader {
		t.Error("First WriteHeader: underlying writer wroteHeader should be true")
	}
	if rw.Header().Get("X-Before") != "true" {
		t.Error("Header set before WriteHeader was lost")
	}

	// Try setting header *after* WriteHeader (should still modify the map)
	rw.Header().Set("X-After", "true")
	if rw.Header().Get("X-After") != "true" {
		t.Error("Header set after WriteHeader failed")
	}

	// Second call - should be ignored by our wrapper
	rw.WriteHeader(http.StatusInternalServerError) // 500
	if rw.statusCode != http.StatusAccepted {
		t.Errorf("Second WriteHeader: got statusCode %d, want %d (should ignore)", rw.statusCode, http.StatusAccepted)
	}
	// Check underlying mock's idempotency
	mock.WriteHeader(http.StatusConflict) // Call underlying mock again
	if mock.statusCode != http.StatusAccepted {
		t.Errorf("Underlying mock WriteHeader was not idempotent, status changed to %d", mock.statusCode)
	}
}

// TestResponseWriter_Write verifies size calculation and default header writing.
func TestResponseWriter_Write(t *testing.T) {
	mock := newMockResponseWriter()
	// Initialize wrapper with default status 200, matching http server behavior
	rw := &responseWriter{ResponseWriter: mock, statusCode: http.StatusOK}

	// Set header before first Write
	rw.Header().Set("Content-Type", "text/plain")

	data1 := []byte("hello")
	n1, err1 := rw.Write(data1)

	// Verifications after first Write
	if err1 != nil {
		t.Fatalf("Write 1 unexpected error: %v", err1)
	}
	if n1 != len(data1) {
		t.Errorf("Write 1: got n=%d, want %d", n1, len(data1))
	}
	if rw.size != int64(len(data1)) {
		t.Errorf("Write 1: got size %d, want %d", rw.size, len(data1))
	}
	// Check if default header was written (implicitly by Write)
	if !rw.wroteHeader {
		t.Error("Write 1: wroteHeader should be true after first Write")
	}
	if rw.statusCode != http.StatusOK {
		t.Errorf("Write 1: got statusCode %d, want %d (default)", rw.statusCode, http.StatusOK)
	}
	// Check underlying mock state
	if !mock.wroteHeader {
		t.Error("Write 1: underlying writer wroteHeader should be true")
	}
	if mock.statusCode != http.StatusOK {
		t.Errorf("Write 1: underlying writer got statusCode %d, want %d", mock.statusCode, http.StatusOK)
	}
	// Check header persisted
	if rw.Header().Get("Content-Type") != "text/plain" {
		t.Error("Header set before Write was lost")
	}

	// Second write
	data2 := []byte(" world")
	n2, err2 := rw.Write(data2)
	if err2 != nil {
		t.Fatalf("Write 2 unexpected error: %v", err2)
	}
	if n2 != len(data2) {
		t.Errorf("Write 2: got n=%d, want %d", n2, len(data2))
	}
	expectedSize := int64(len(data1) + len(data2))
	if rw.size != expectedSize {
		t.Errorf("Write 2: got size %d, want %d", rw.size, expectedSize)
	}

	// Check final body content
	if mock.body.String() != "hello world" {
		t.Errorf("Body content mismatch: got %q, want %q", mock.body.String(), "hello world")
	}
}

// TestResponseWriter_Write_After_WriteHeader verifies behavior when WriteHeader is called explicitly first.
func TestResponseWriter_Write_After_WriteHeader(t *testing.T) {
	mock := newMockResponseWriter()
	rw := &responseWriter{ResponseWriter: mock}

	// Explicitly write header first
	rw.WriteHeader(http.StatusCreated)           // 201
	rw.Header().Set("Location", "/new-resource") // Set header after WriteHeader

	data := []byte("resource created")
	n, err := rw.Write(data)

	// Verifications
	if err != nil {
		t.Fatalf("Write unexpected error: %v", err)
	}
	if n != len(data) {
		t.Errorf("Write: got n=%d, want %d", n, len(data))
	}
	if rw.size != int64(len(data)) {
		t.Errorf("Write: got size %d, want %d", rw.size, len(data))
	}
	// Ensure status code wasn't overwritten by Write's default logic
	if rw.statusCode != http.StatusCreated {
		t.Errorf("Write after WriteHeader: got statusCode %d, want %d", rw.statusCode, http.StatusCreated)
	}
	// Check underlying mock status
	if mock.statusCode != http.StatusCreated {
		t.Errorf("Write after WriteHeader: underlying writer got statusCode %d, want %d", mock.statusCode, http.StatusCreated)
	}
	// Check header
	if rw.Header().Get("Location") != "/new-resource" {
		t.Error("Header set after WriteHeader was lost or incorrect")
	}
	// Check body
	if mock.body.String() != string(data) {
		t.Errorf("Body content mismatch: got %q, want %q", mock.body.String(), string(data))
	}
}

// TestExtractIP verifies parsing of IP addresses from RemoteAddr strings.
func TestExtractIP(t *testing.T) {
	testCases := []struct {
		remoteAddr string
		wantIP     string
	}{
		{"192.0.2.1:12345", "192.0.2.1"},
		{"192.0.2.1", "192.0.2.1"}, // No port
		{"[::1]:8080", "::1"},
		{"[::1]", "::1"}, // No port, valid IPv6
		{"[2001:db8::1]:1234", "2001:db8::1"},
		{"2001:db8::1", "2001:db8::1"},                   // No brackets, no port, valid IPv6
		{"example.com:80", "example.com:80"},             // Hostname, not IP -> return original
		{"localhost:1234", "localhost:1234"},             // Hostname -> return original
		{"", ""},                                         // Empty input
		{"invalid-address", "invalid-address"},           // Cannot parse as IP or host:port
		{"/var/run/docker.sock", "/var/run/docker.sock"}, // Unix socket path
		{"[::1]:invalidport", "::1"},                     // Valid IP, invalid port -> extract IP
		{"[invalidipv6]:8080", "[invalidipv6]:8080"},     // Invalid IP in brackets -> return original
		{"192.0.2.1:invalid", "192.0.2.1"},               // Valid IP, invalid port -> extract IP
		{"192.0.2.invalid:1234", "192.0.2.invalid:1234"}, // Invalid IP, valid port -> return original
	}

	for _, tc := range testCases {
		t.Run(tc.remoteAddr, func(t *testing.T) {
			gotIP := extractIP(tc.remoteAddr)
			if gotIP != tc.wantIP {
				t.Errorf("extractIP(%q) = %q, want %q", tc.remoteAddr, gotIP, tc.wantIP)
			}
		})
	}
}

// mockLogHandler captures slog records for verification.
type mockLogHandler struct {
	mu      sync.Mutex
	records []slog.Record // Shared slice pointer for simplicity in test mocks
	attrs   []slog.Attr
	group   string
}

func (h *mockLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *mockLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Make a copy of the record to avoid data races if the caller modifies it later.
	recordCopy := r
	h.records = append(h.records, recordCopy)
	return nil
}
func (h *mockLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Create a new handler instance instead of copying the mutex.
	h2 := &mockLogHandler{
		records: h.records, // Share underlying slice pointer
		attrs:   make([]slog.Attr, len(h.attrs)+len(attrs)),
		group:   h.group,
	}
	copy(h2.attrs, h.attrs)
	copy(h2.attrs[len(h.attrs):], attrs)
	return h2
}
func (h *mockLogHandler) WithGroup(name string) slog.Handler {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Create a new handler instance instead of copying the mutex.
	h2 := &mockLogHandler{
		records: h.records, // Share underlying slice pointer
		attrs:   make([]slog.Attr, len(h.attrs)),
	}
	copy(h2.attrs, h.attrs)
	// Simplistic group handling for test purposes.
	if h.group != "" {
		h2.group = h.group + "." + name
	} else {
		h2.group = name
	}
	return h2
}
func (h *mockLogHandler) GetRecords() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Return a copy to prevent modification by the caller.
	out := make([]slog.Record, len(h.records))
	copy(out, h.records)
	return out
}
func (h *mockLogHandler) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = nil
	h.attrs = nil
	h.group = ""
}

// TestMiddleware verifies the core logging behavior of the Middleware.
func TestMiddleware(t *testing.T) {
	// Set up global propagator for trace context tests
	prop := propagation.TraceContext{}
	origProp := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(prop)
	t.Cleanup(func() { otel.SetTextMapPropagator(origProp) })

	traceIDHex := "11111111111111112222222222222222"
	spanIDHex := "3333333333333333"
	traceParentHeader := fmt.Sprintf("00-%s-%s-01", traceIDHex, spanIDHex) // Sampled

	testCases := []struct {
		name                       string
		requestMethod              string
		requestURL                 string
		requestHeaders             map[string]string
		requestBody                string
		remoteAddr                 string
		handlerStatusCode          int
		handlerBody                string
		handlerExplicitWriteHeader bool
		wantLogLevel               slog.Level
		wantStatus                 int
		wantRequestSize            int64
		wantResponseSize           int64
		wantUserAgent              string
		wantReferer                string
		wantRemoteIP               string
		wantTraceCheck             func(*testing.T, context.Context)
	}{
		{
			name:                       "GET 200 OK",
			requestMethod:              "GET",
			requestURL:                 "/path/ok",
			remoteAddr:                 "192.0.2.10:1234",
			handlerStatusCode:          http.StatusOK,
			handlerBody:                "OK Body",
			handlerExplicitWriteHeader: true,
			wantLogLevel:               slog.LevelInfo,
			wantStatus:                 http.StatusOK,
			wantRequestSize:            0,
			wantResponseSize:           int64(len("OK Body")),
			wantRemoteIP:               "192.0.2.10",
		},
		{
			name:                       "POST 201 Created",
			requestMethod:              "POST",
			requestURL:                 "/path/create",
			requestBody:                `{"key":"value"}`,
			remoteAddr:                 "192.0.2.20:5678",
			handlerStatusCode:          http.StatusCreated,
			handlerBody:                "Created",
			handlerExplicitWriteHeader: true,
			wantLogLevel:               slog.LevelInfo,
			wantStatus:                 http.StatusCreated,
			wantRequestSize:            int64(len(`{"key":"value"}`)),
			wantResponseSize:           int64(len("Created")),
			wantRemoteIP:               "192.0.2.20",
		},
		{
			name:                       "GET 404 Not Found",
			requestMethod:              "GET",
			requestURL:                 "/path/missing",
			remoteAddr:                 "10.0.0.1:80",
			handlerStatusCode:          http.StatusNotFound,
			handlerBody:                "Not Found",
			handlerExplicitWriteHeader: true,
			wantLogLevel:               slog.LevelWarn,
			wantStatus:                 http.StatusNotFound,
			wantRequestSize:            0,
			wantResponseSize:           int64(len("Not Found")),
			wantRemoteIP:               "10.0.0.1",
		},
		{
			name:                       "GET 500 Internal Error",
			requestMethod:              "GET",
			requestURL:                 "/path/error",
			remoteAddr:                 "10.0.0.2:8080",
			handlerStatusCode:          http.StatusInternalServerError,
			handlerBody:                "Server Error",
			handlerExplicitWriteHeader: true,
			wantLogLevel:               slog.LevelError,
			wantStatus:                 http.StatusInternalServerError,
			wantRequestSize:            0,
			wantResponseSize:           int64(len("Server Error")),
			wantRemoteIP:               "10.0.0.2",
		},
		{
			name:          "With Headers",
			requestMethod: "GET",
			requestURL:    "/with/headers",
			requestHeaders: map[string]string{
				"User-Agent": "TestAgent/1.0",
				"Referer":    "http://example.com/previous",
			},
			remoteAddr:                 "172.16.0.1:9000",
			handlerStatusCode:          http.StatusOK,
			handlerBody:                "Headers OK",
			handlerExplicitWriteHeader: true,
			wantLogLevel:               slog.LevelInfo,
			wantStatus:                 http.StatusOK,
			wantRequestSize:            0,
			wantResponseSize:           int64(len("Headers OK")),
			wantUserAgent:              "TestAgent/1.0",
			wantReferer:                "http://example.com/previous",
			wantRemoteIP:               "172.16.0.1",
		},
		{
			name:          "With TraceParent",
			requestMethod: "GET",
			requestURL:    "/trace",
			requestHeaders: map[string]string{
				"traceparent": traceParentHeader,
			},
			remoteAddr:                 "192.168.1.100:1111",
			handlerStatusCode:          http.StatusOK,
			handlerBody:                "Trace OK",
			handlerExplicitWriteHeader: true,
			wantLogLevel:               slog.LevelInfo,
			wantStatus:                 http.StatusOK,
			wantRequestSize:            0,
			wantResponseSize:           int64(len("Trace OK")),
			wantRemoteIP:               "192.168.1.100",
			wantTraceCheck: func(t *testing.T, ctx context.Context) {
				t.Helper()
				sc := trace.SpanContextFromContext(ctx)
				if !sc.IsValid() {
					t.Error("Expected valid SpanContext in inner handler")
					return
				}
				if sc.TraceID().String() != traceIDHex {
					t.Errorf("Inner handler TraceID mismatch: got %s, want %s", sc.TraceID().String(), traceIDHex)
				}
				if sc.SpanID().String() != spanIDHex {
					t.Errorf("Inner handler SpanID mismatch: got %s, want %s", sc.SpanID().String(), spanIDHex)
				}
				if !sc.IsSampled() {
					t.Error("Inner handler IsSampled mismatch: got false, want true")
				}
			},
		},
		{
			name:                       "IPv6 Remote Addr",
			requestMethod:              "GET",
			requestURL:                 "/ipv6",
			remoteAddr:                 "[2001:db8::1]:12345",
			handlerStatusCode:          http.StatusOK,
			handlerBody:                "IPv6 OK",
			handlerExplicitWriteHeader: true,
			wantLogLevel:               slog.LevelInfo,
			wantStatus:                 http.StatusOK,
			wantRequestSize:            0,
			wantResponseSize:           int64(len("IPv6 OK")),
			wantRemoteIP:               "2001:db8::1",
		},
		{
			name:                       "Implicit WriteHeader",
			requestMethod:              "GET",
			requestURL:                 "/implicit",
			remoteAddr:                 "192.0.2.50:8888",
			handlerStatusCode:          http.StatusOK, // Status code to check against
			handlerBody:                "Implicit OK",
			handlerExplicitWriteHeader: false, // Handler only calls Write()
			wantLogLevel:               slog.LevelInfo,
			wantStatus:                 http.StatusOK, // Expect 200 logged
			wantRequestSize:            0,
			wantResponseSize:           int64(len("Implicit OK")),
			wantRemoteIP:               "192.0.2.50",
		},
	}

	// Options for comparing logging.HTTPRequest top-level fields
	httpTopLevelCmpOpts := []cmp.Option{
		cmpopts.IgnoreFields(logging.HTTPRequest{}, "Request", "Latency"),
	}
	// Options for comparing the embedded http.Request
	httpEmbeddedReqCmpOpts := []cmp.Option{
		cmpopts.IgnoreUnexported(http.Request{}),
		// Only compare specific fields we care about
		cmpopts.IgnoreFields(http.Request{}, "Body", "GetBody", "TLS", "Cancel", "Response", "ctx", "URL", "ContentLength", "Header"), // URL is parsed into RequestURI by server
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mockHandler := &mockLogHandler{}
			logger := slog.New(mockHandler)
			var handlerCtx context.Context // To capture context passed to inner handler

			// Inner handler
			innerHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handlerCtx = r.Context() // Capture the context
				if tc.handlerExplicitWriteHeader {
					w.WriteHeader(tc.handlerStatusCode)
				}
				_, _ = w.Write([]byte(tc.handlerBody))
			})

			// Apply middleware
			wrappedHandler := Middleware(logger)(innerHandler)

			// Create request
			var bodyReader io.Reader
			if tc.requestBody != "" {
				bodyReader = strings.NewReader(tc.requestBody)
			}
			req := httptest.NewRequest(tc.requestMethod, tc.requestURL, bodyReader)
			req.RemoteAddr = tc.remoteAddr
			if tc.requestBody != "" {
				req.ContentLength = int64(len(tc.requestBody)) // Set ContentLength explicitly
				req.Header.Set("Content-Length", fmt.Sprintf("%d", len(tc.requestBody)))
			}
			for k, v := range tc.requestHeaders {
				req.Header.Set(k, v)
			}

			// Create recorder
			recorder := httptest.NewRecorder()

			// Execute
			wrappedHandler.ServeHTTP(recorder, req)

			// Check response
			if recorder.Code != tc.handlerStatusCode {
				t.Errorf("Response status code = %d, want %d", recorder.Code, tc.handlerStatusCode)
			}
			if recorder.Body.String() != tc.handlerBody {
				t.Errorf("Response body = %q, want %q", recorder.Body.String(), tc.handlerBody)
			}

			// Check logs
			records := mockHandler.GetRecords()
			if len(records) != 1 {
				t.Fatalf("Expected 1 log record, got %d", len(records))
			}
			record := records[0]

			if record.Level != tc.wantLogLevel {
				t.Errorf("Log level = %v, want %v", record.Level, tc.wantLogLevel)
			}
			if record.Message != "HTTP request processed" {
				t.Errorf("Log message = %q, want %q", record.Message, "HTTP request processed")
			}

			// Check attributes
			var loggedHTTPRequest *logging.HTTPRequest
			var loggedDuration time.Duration
			var foundHTTPReq, foundDuration bool

			record.Attrs(func(a slog.Attr) bool {
				switch a.Key {
				case httpRequestKey:
					if reqPtr, ok := a.Value.Any().(*logging.HTTPRequest); ok {
						loggedHTTPRequest = reqPtr
						foundHTTPReq = true
					} else {
						t.Errorf("Attribute %q has wrong type: got %T, want *logging.HTTPRequest", httpRequestKey, a.Value.Any())
					}
				case "duration":
					if d, ok := a.Value.Any().(time.Duration); ok {
						loggedDuration = d
						foundDuration = true
					} else {
						t.Errorf("Attribute 'duration' has wrong type: got %T, want time.Duration", a.Value.Any())
					}
				}
				return true
			})

			if !foundHTTPReq {
				t.Fatalf("Missing required log attribute: %q", httpRequestKey)
			}
			if !foundDuration {
				t.Fatalf("Missing required log attribute: 'duration'")
			}
			if loggedDuration < 0 {
				t.Errorf("Logged duration = %v, want >= 0", loggedDuration)
			}

			// Construct expected top-level HTTPRequest struct fields
			expectedHTTPRequestTopLevel := &logging.HTTPRequest{
				RequestSize:  tc.wantRequestSize,
				Status:       tc.wantStatus,
				ResponseSize: tc.wantResponseSize,
				RemoteIP:     tc.wantRemoteIP,
			}

			// Compare top-level HTTPRequest fields
			if diff := cmp.Diff(expectedHTTPRequestTopLevel, loggedHTTPRequest, httpTopLevelCmpOpts...); diff != "" {
				t.Errorf("Logged HTTPRequest top-level fields mismatch (-want +got):\n%s", diff)
			}

			// Verify embedded request details
			if loggedHTTPRequest.Request == nil {
				t.Fatal("Logged HTTPRequest.Request is nil")
			}

			// Construct expected embedded http.Request
			expectedEmbeddedRequest := &http.Request{
				Method:     tc.requestMethod,
				RequestURI: tc.requestURL, // httptest server sets RequestURI
				Proto:      "HTTP/1.1",    // Default from httptest
				ProtoMajor: 1,
				ProtoMinor: 1,
				Host:       "example.com", // Default host used by httptest
				RemoteAddr: tc.remoteAddr,
			}

			// Compare embedded http.Request using cmp.Diff and specific options
			if diff := cmp.Diff(expectedEmbeddedRequest, loggedHTTPRequest.Request, httpEmbeddedReqCmpOpts...); diff != "" {
				t.Errorf("Logged HTTPRequest.Request mismatch (-want +got):\n%s", diff)
			}

			// Check trace context if applicable
			if tc.wantTraceCheck != nil {
				if handlerCtx == nil {
					t.Fatal("Inner handler context was not captured")
				}
				tc.wantTraceCheck(t, handlerCtx)
			}
		})
	}
}
