package http

import (
	"bytes"
	"net/http"
	"testing"
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

// Unit testing the Middleware function is complex due to its dependencies
// on http.Handler flow, slog.Logger behavior, and OTel context extraction.
// It requires extensive mocking that falls outside the scope of simple unit tests.
// Integration testing is recommended for verifying the Middleware's end-to-end behavior.
