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

package loggingmock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pjscruggs/slogcp"
	slogcpgrpc "github.com/pjscruggs/slogcp/grpc"
	slogcphttp "github.com/pjscruggs/slogcp/http"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// TestSlogcpLogEntryTransforms exercises slogcp JSON output against the logging mock transformer.
func TestSlogcpLogEntryTransforms(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(
		io.Discard,
		slogcp.WithRedirectWriter(&buf),
		slogcp.WithSeverityAliases(true),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	logger := slog.New(h)
	req := &slogcp.HTTPRequest{
		RequestMethod:  "GET",
		RequestURL:     "https://example.com/orders/123",
		RequestSize:    128,
		Status:         200,
		ResponseSize:   512,
		RemoteIP:       "203.0.113.1",
		LocalIP:        "10.0.0.5",
		UserAgent:      "integration-test/1.0",
		CacheHit:       true,
		CacheLookup:    true,
		CacheFillBytes: 64,
	}

	logger.WarnContext(
		context.Background(),
		"order failed",
		slog.Int("attempt", 3),
		slog.Any("httpRequest", req),
		slog.Group(
			slogcp.LabelsGroup,
			slog.String("component", "payments"),
			slog.String("region", "us-central1"),
		),
	)

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected log output")
	}

	if err := EnsureJSONObject(line); err != nil {
		t.Fatalf("EnsureJSONObject() returned %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		t.Fatalf("json.Unmarshal() returned %v", err)
	}

	if got := raw["severity"]; got != "W" {
		t.Fatalf("raw severity = %v, want W", got)
	}
	if got := raw["message"]; got != "order failed" {
		t.Fatalf("raw message = %v, want order failed", got)
	}

	if got, ok := raw["attempt"].(float64); !ok || got != 3 {
		t.Fatalf("raw attempt = %v (type %T), want float64(3)", raw["attempt"], raw["attempt"])
	}

	rawLabels, ok := raw[slogcp.LabelsGroup].(map[string]any)
	if !ok {
		t.Fatalf("raw labels missing or wrong type: %T", raw[slogcp.LabelsGroup])
	}
	if got := rawLabels["component"]; got != "payments" {
		t.Fatalf("raw labels component = %v, want payments", got)
	}
	if got := rawLabels["region"]; got != "us-central1" {
		t.Fatalf("raw labels region = %v, want us-central1", got)
	}

	rawHTTP, ok := raw["httpRequest"].(map[string]any)
	if !ok {
		t.Fatalf("raw httpRequest missing or wrong type: %T", raw["httpRequest"])
	}
	if got := rawHTTP["requestMethod"]; got != "GET" {
		t.Fatalf("raw requestMethod = %v, want GET", got)
	}
	if got := rawHTTP["requestUrl"]; got != "https://example.com/orders/123" {
		t.Fatalf("raw requestUrl = %v, want https://example.com/orders/123", got)
	}
	if got := rawHTTP["status"]; got != float64(200) {
		t.Fatalf("raw status = %v, want 200", got)
	}
	if got := rawHTTP["requestSize"]; got != "128" {
		t.Fatalf("raw requestSize = %v, want 128", got)
	}
	if got := rawHTTP["responseSize"]; got != "512" {
		t.Fatalf("raw responseSize = %v, want 512", got)
	}

	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	// Wrap in jsonPayload to simulate the logging agent
	wrapped := fmt.Sprintf(`{"jsonPayload":%s}`, line)
	transformed, err := TransformLogEntryJSON(wrapped, now)
	if err != nil {
		t.Fatalf("TransformLogEntryJSON() returned %v", err)
	}
	entry := mustUnmarshalMap(t, transformed)

	if got := entry["timestamp"]; got != formatRFC3339ZNormalized(now) {
		t.Fatalf("transformed timestamp = %v, want %s", got, formatRFC3339ZNormalized(now))
	}
	if got := entry["severity"]; got != "WARNING" {
		t.Fatalf("transformed severity = %v, want WARNING", got)
	}

	payload, ok := entry["jsonPayload"].(map[string]any)
	if !ok {
		t.Fatal("jsonPayload missing in transformed output")
	}

	if got := payload["message"]; got != "order failed" {
		t.Fatalf("transformed message = %v, want order failed", got)
	}

	if got, ok := payload["attempt"].(float64); !ok || got != 3 {
		t.Fatalf("transformed attempt = %v (type %T), want float64(3)", payload["attempt"], payload["attempt"])
	}

	labels, ok := entry["labels"].(map[string]any)
	if !ok {
		t.Fatalf("transformed labels missing or wrong type: %T", entry["labels"])
	}
	if got := labels["component"]; got != "payments" {
		t.Fatalf("transformed labels component = %v, want payments", got)
	}
	if got := labels["region"]; got != "us-central1" {
		t.Fatalf("transformed labels region = %v, want us-central1", got)
	}

	httpReq, ok := entry["httpRequest"].(map[string]any)
	if !ok {
		t.Fatalf("transformed httpRequest missing or wrong type: %T", entry["httpRequest"])
	}
	if got := httpReq["requestMethod"]; got != "GET" {
		t.Fatalf("transformed requestMethod = %v, want GET", got)
	}
	if got := httpReq["requestUrl"]; got != "https://example.com/orders/123" {
		t.Fatalf("transformed requestUrl = %v, want https://example.com/orders/123", got)
	}
	if got := httpReq["status"]; got != float64(200) {
		t.Fatalf("transformed status = %v, want 200", got)
	}
	if got := httpReq["requestSize"]; got != "128" {
		t.Fatalf("transformed requestSize = %v, want 128", got)
	}
	if got := httpReq["responseSize"]; got != "512" {
		t.Fatalf("transformed responseSize = %v, want 512", got)
	}
}

// TestHTTPRequestAttrCoexistsWithHTTPAttributes ensures httpRequest and http.* attributes appear together.
func TestHTTPRequestAttrCoexistsWithHTTPAttributes(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(
		io.Discard,
		slogcp.WithRedirectWriter(&buf),
		slogcp.WithTraceProjectID("proj-123"),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	baseLogger := slog.New(h)

	var capturedLogger *slog.Logger
	var capturedScope *slogcphttp.RequestScope

	app := slogcphttp.Middleware(
		slogcphttp.WithLogger(baseLogger),
		slogcphttp.WithProjectID("proj-123"),
		slogcphttp.WithOTel(false),
		slogcphttp.WithIncludeQuery(true),
		slogcphttp.WithUserAgent(true),
	)(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		scope, ok := slogcphttp.ScopeFromContext(r.Context())
		if !ok {
			t.Fatalf("ScopeFromContext missing")
		}
		capturedScope = scope
		capturedLogger = slogcp.Logger(r.Context())

		w.WriteHeader(stdhttp.StatusAccepted)
		if _, err := w.Write([]byte("ok")); err != nil {
			t.Fatalf("write response: %v", err)
		}
	}))

	req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com/widgets/42?q=blue", stdhttp.NoBody)
	req.RemoteAddr = "198.51.100.50:9000"
	req.Header.Set("User-Agent", "middleware-test/1.0")

	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)

	if capturedLogger == nil || capturedScope == nil {
		t.Fatalf("request logger or scope missing")
	}

	capturedLogger.InfoContext(context.Background(),
		"post-request",
		slog.String("custom", "value"),
		slogcphttp.HTTPRequestAttr(req, capturedScope),
	)

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatalf("expected log output")
	}
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	transformed := transformLine(t, line, now)

	httpReq, ok := transformed["httpRequest"].(map[string]any)
	if !ok {
		t.Fatalf("httpRequest missing or wrong type: %T", transformed["httpRequest"])
	}
	if got := httpReq["requestMethod"]; got != "GET" {
		t.Fatalf("requestMethod = %v, want GET", got)
	}
	if got := httpReq["status"]; got != float64(stdhttp.StatusAccepted) {
		t.Fatalf("status = %v, want %d", got, stdhttp.StatusAccepted)
	}
	if got := httpReq["requestSize"]; got != "0" {
		t.Fatalf("requestSize = %v, want 0", got)
	}
	if got := httpReq["responseSize"]; got != "2" {
		t.Fatalf("responseSize = %v, want 2", got)
	}
	if got := httpReq["remoteIp"]; got != "198.51.100.50" {
		t.Fatalf("remoteIp = %v, want 198.51.100.50", got)
	}

	payload, ok := transformed["jsonPayload"].(map[string]any)
	if !ok {
		t.Fatalf("jsonPayload missing or wrong type: %T (%v)", transformed["jsonPayload"], transformed)
	}
	if got := payload["http.method"]; got != "GET" {
		t.Fatalf("http.method = %v, want GET", got)
	}
	if got := payload["http.target"]; got != "/widgets/42" {
		t.Fatalf("http.target = %v, want /widgets/42", got)
	}
	if got := payload["custom"]; got != "value" {
		t.Fatalf("custom attr = %v, want value", got)
	}
}

// transformLine feeds a raw log line through the logging mock transformer and parses the result.
func transformLine(t *testing.T, line string, now time.Time) map[string]any {
	t.Helper()
	wrapped := `{"jsonPayload":` + line + `}`
	out, err := TransformLogEntryJSON(wrapped, now)
	if err != nil {
		t.Fatalf("TransformLogEntryJSON returned %v", err)
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(out), &entry); err != nil {
		t.Fatalf("json.Unmarshal() returned %v", err)
	}
	return entry
}

// TestTraceContextPropagation verifies that trace context headers are correctly propagated to logs.
func TestTraceContextPropagation(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(
		io.Discard,
		slogcp.WithRedirectWriter(&buf),
		slogcp.WithTraceProjectID("my-project"),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v", err)
	}
	t.Cleanup(func() { h.Close() })

	baseLogger := slog.New(h)

	// Middleware chain: InjectTraceContext -> slogcp Middleware -> Handler
	handler := slogcphttp.InjectTraceContextMiddleware()(
		slogcphttp.Middleware(
			slogcphttp.WithLogger(baseLogger),
			slogcphttp.WithProjectID("my-project"),
		)(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
			slogcp.Logger(r.Context()).InfoContext(r.Context(), "trace test")
		})),
	)

	// 105445aa7843bc8bf206b12000100000 is a 32-char hex trace ID
	// 1 is the span ID (decimal)
	// o=1 means sampled
	traceHeader := "105445aa7843bc8bf206b12000100000/1;o=1"
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Cloud-Trace-Context", traceHeader)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected log output")
	}

	lines := strings.Split(line, "\n")
	var traceLog map[string]any
	for _, l := range lines {
		if strings.Contains(l, "trace test") {
			traceLog = transformLine(t, l, time.Now())
			break
		}
	}

	if traceLog == nil {
		t.Logf("All lines: %s", line)
		t.Fatal("did not find 'trace test' log entry")
	}

	expectedTrace := "projects/my-project/traces/105445aa7843bc8bf206b12000100000"
	if got, ok := traceLog["trace"].(string); !ok || got != expectedTrace {
		t.Errorf("trace = %v, want %v. Full log: %+v", got, expectedTrace, traceLog)
	}

	if got, ok := traceLog["traceSampled"].(bool); !ok || !got {
		t.Errorf("traceSampled = %v, want true", got)
	}
}

// TestErrorReportingIntegration verifies that errors are logged with stack traces in the correct format.
func TestErrorReportingIntegration(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(
		io.Discard,
		slogcp.WithRedirectWriter(&buf),
		slogcp.WithStackTraceEnabled(true),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v", err)
	}
	t.Cleanup(func() { h.Close() })

	logger := slog.New(h)
	logger.Error("something went wrong")

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected log output")
	}

	wrapped := fmt.Sprintf(`{"jsonPayload":%s}`, line)
	transformed, err := TransformLogEntryJSON(wrapped, time.Now())
	if err != nil {
		t.Fatalf("TransformLogEntryJSON() returned %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(transformed), &result); err != nil {
		t.Fatalf("json.Unmarshal() returned %v", err)
	}

	// Verify stack_trace is present in jsonPayload or top-level.
	payload, ok := result["jsonPayload"].(map[string]any)
	if !ok {
		t.Fatalf("jsonPayload missing")
	}

	if _, ok := payload["stack_trace"]; !ok {
		// Try top level just in case
		if _, ok := result["stack_trace"]; !ok {
			t.Errorf("stack_trace missing from log entry")
		}
	}
}

// TestGRPCInterceptorIntegration verifies that the gRPC interceptor correctly logs requests.
func TestGRPCInterceptorIntegration(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(
		io.Discard,
		slogcp.WithRedirectWriter(&buf),
		slogcp.WithTraceProjectID("grpc-proj"),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v", err)
	}
	t.Cleanup(func() { h.Close() })

	logger := slog.New(h)

	interceptor := slogcpgrpc.UnaryServerInterceptor(
		slogcpgrpc.WithLogger(logger),
		slogcpgrpc.WithProjectID("grpc-proj"),
	)

	// Mock handler that logs something
	mockHandler := func(ctx context.Context, req any) (any, error) {
		slogcp.Logger(ctx).Info("grpc handler")
		return "response", nil
	}

	info := &grpc.UnaryServerInfo{
		FullMethod: "/myservice.MyService/MyMethod",
	}

	// Create context with metadata (simulating incoming request)
	md := metadata.New(map[string]string{
		"x-cloud-trace-context": "105445aa7843bc8bf206b12000100000/1;o=1",
	})
	ctx := metadata.NewIncomingContext(context.Background(), md)

	_, err = interceptor(ctx, "request", info, mockHandler)
	if err != nil {
		t.Fatalf("interceptor failed: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected log output")
	}

	// Expect "grpc handler" log.
	lines := strings.Split(line, "\n")
	var handlerLog map[string]any
	for _, l := range lines {
		wrapped := fmt.Sprintf(`{"jsonPayload":%s}`, l)
		entry, err := TransformLogEntryJSON(wrapped, time.Now())
		if err != nil {
			t.Fatalf("TransformLogEntryJSON returned %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(entry), &parsed); err != nil {
			t.Fatalf("json.Unmarshal() returned %v", err)
		}

		if msg, ok := parsed["message"].(string); ok && msg == "grpc handler" {
			handlerLog = parsed
			break
		}
		// Check inside jsonPayload
		if payload, ok := parsed["jsonPayload"].(map[string]any); ok {
			if msg, ok := payload["message"].(string); ok && msg == "grpc handler" {
				handlerLog = parsed
				break
			}
		}
	}

	if handlerLog == nil {
		t.Logf("All lines: %s", line)
		t.Fatal("did not find 'grpc handler' log")
	}

	// Check trace propagation
	expectedTrace := "projects/grpc-proj/traces/105445aa7843bc8bf206b12000100000"
	if got := handlerLog["trace"]; got != expectedTrace {
		t.Errorf("trace = %v, want %v", got, expectedTrace)
	}
}
