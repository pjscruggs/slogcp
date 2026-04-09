// Copyright 2025-2026 Patrick J. Scruggs
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

package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// TargetClient is the HTTP client for the target application
type TargetClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewTargetClient creates a new target client
func NewTargetClient(ctx context.Context, baseURL string) (*TargetClient, error) {
	httpClient, err := newTargetHTTPClient(ctx, baseURL)
	if err != nil {
		return nil, err
	}

	return &TargetClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
	}, nil
}

// LogRequest represents a log request payload
type LogRequest struct {
	Message    string            `json:"message"`
	TestID     string            `json:"test_id"`
	Attributes map[string]any    `json:"attributes,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Operation  *Operation        `json:"operation,omitempty"`
}

// LogResponse represents the response from a log endpoint
type LogResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// Operation represents Cloud Logging operation grouping metadata in requests.
type Operation struct {
	ID       string `json:"id"`
	Producer string `json:"producer,omitempty"`
	First    bool   `json:"first,omitempty"`
	Last     bool   `json:"last,omitempty"`
}

// TraceRequest represents a trace test request
type TraceRequest struct {
	TestID  string         `json:"test_id"`
	Message string         `json:"message"`
	Options map[string]any `json:"options,omitempty"`
}

// TraceServerStreamRequest configures the payload sent to the trace target's
// server-streaming endpoint.
type TraceServerStreamRequest struct {
	TraceRequest    TraceRequest `json:"trace_request"`
	ResponseCount   int          `json:"response_count"`
	ResponsePayload string       `json:"response_payload,omitempty"`
	FinalStatus     string       `json:"final_status,omitempty"`
	FinalMessage    string       `json:"final_message,omitempty"`
}

// TraceClientStreamChunk represents a single client-streaming message.
type TraceClientStreamChunk struct {
	TestID       string `json:"test_id,omitempty"`
	ChunkID      string `json:"chunk_id,omitempty"`
	Payload      string `json:"payload,omitempty"`
	FinalStatus  string `json:"final_status,omitempty"`
	FinalMessage string `json:"final_message,omitempty"`
}

// TraceClientStreamRequest configures the client-streaming trace request.
type TraceClientStreamRequest struct {
	TraceRequest TraceRequest             `json:"trace_request"`
	Chunks       []TraceClientStreamChunk `json:"chunks"`
}

// TraceBidiStreamMessage represents a single bidirectional streaming message.
type TraceBidiStreamMessage struct {
	TestID       string `json:"test_id,omitempty"`
	ChunkID      string `json:"chunk_id,omitempty"`
	Payload      string `json:"payload,omitempty"`
	CloseAfter   bool   `json:"close_after,omitempty"`
	FinalStatus  string `json:"final_status,omitempty"`
	FinalMessage string `json:"final_message,omitempty"`
}

// TraceBidiStreamRequest configures the bidirectional streaming trace request.
type TraceBidiStreamRequest struct {
	TraceRequest TraceRequest             `json:"trace_request"`
	Messages     []TraceBidiStreamMessage `json:"messages"`
}

// TraceResponse represents the response from a trace endpoint
type TraceResponse struct {
	Success bool              `json:"success"`
	Message string            `json:"message"`
	TraceID string            `json:"trace_id,omitempty"`
	SpanID  string            `json:"spanId,omitempty"`
	Details map[string]string `json:"details,omitempty"`
}

// HealthCheck performs a health check on the target app
func (c *TargetClient) HealthCheck(ctx context.Context) error {
	return c.HealthCheckWithPath(ctx, "/health")
}

// HealthCheckWithPath performs a health check on the target app using the provided path.
// The supplied path must be relative and may include query parameters.
func (c *TargetClient) HealthCheckWithPath(ctx context.Context, path string) error {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("creating health check request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("health check request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned status %d", resp.StatusCode)
	}

	return nil
}

// LogWithSeverity sends a log with the specified severity level
func (c *TargetClient) LogWithSeverity(ctx context.Context, severity string, logReq LogRequest) (*LogResponse, error) {
	endpoint := fmt.Sprintf("/log/%s", severity)
	return c.sendLogRequest(ctx, endpoint, logReq)
}

// LogStructured sends a structured log
func (c *TargetClient) LogStructured(ctx context.Context, logReq LogRequest) (*LogResponse, error) {
	return c.sendLogRequest(ctx, "/log/structured", logReq)
}

// LogNested sends a log with nested structure
func (c *TargetClient) LogNested(ctx context.Context, logReq LogRequest) (*LogResponse, error) {
	return c.sendLogRequest(ctx, "/log/nested", logReq)
}

// LogWithLabels sends a log with custom labels
func (c *TargetClient) LogWithLabels(ctx context.Context, logReq LogRequest) (*LogResponse, error) {
	return c.sendLogRequest(ctx, "/log/labels", logReq)
}

// LogWithOperation sends a log that includes Cloud Logging operation metadata.
func (c *TargetClient) LogWithOperation(ctx context.Context, logReq LogRequest) (*LogResponse, error) {
	return c.sendLogRequest(ctx, "/log/operation", logReq)
}

// LogBatch sends multiple logs in a batch
func (c *TargetClient) LogBatch(ctx context.Context, logReq LogRequest) (*LogResponse, error) {
	return c.sendLogRequest(ctx, "/log/batch", logReq)
}

// LogHTTPRequest emits a log that includes a Cloud Logging httpRequest payload.
func (c *TargetClient) LogHTTPRequest(ctx context.Context, logReq LogRequest) (*LogResponse, error) {
	return c.sendLogRequest(ctx, "/log/http-request", logReq)
}

// LogHTTPRequestInflight emits a log that captures httpRequest metadata mid-request.
func (c *TargetClient) LogHTTPRequestInflight(ctx context.Context, logReq LogRequest) (*LogResponse, error) {
	return c.sendLogRequest(ctx, "/log/http-request-inflight", logReq)
}

// LogHTTPRequestParserResidual emits a parser-probe log with known + unknown
// httpRequest fields so backend promotion and residual behavior can be observed.
func (c *TargetClient) LogHTTPRequestParserResidual(ctx context.Context, logReq LogRequest) (*LogResponse, error) {
	return c.sendLogRequest(ctx, "/log/http-request-parser-residual", logReq)
}

// LogHTTPRequestParserStatusZero emits a parser-probe log with status=0 so
// backend status omission behavior can be observed.
func (c *TargetClient) LogHTTPRequestParserStatusZero(ctx context.Context, logReq LogRequest) (*LogResponse, error) {
	return c.sendLogRequest(ctx, "/log/http-request-parser-status-zero", logReq)
}

// sendLogRequest is a helper to send log requests
func (c *TargetClient) sendLogRequest(ctx context.Context, endpoint string, logReq LogRequest) (*LogResponse, error) {
	body, err := json.Marshal(logReq)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request returned status %d", resp.StatusCode)
	}

	var logResp LogResponse
	if err := json.NewDecoder(resp.Body).Decode(&logResp); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &logResp, nil
}

// DoRequest issues an HTTP request against the target application and returns the
// raw response for further inspection by tests. The caller is responsible for
// closing the response body.
func (c *TargetClient) DoRequest(ctx context.Context, method, path string, headers map[string]string, body []byte) (*http.Response, error) {
	if method == "" {
		method = http.MethodGet
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	var requestBody io.Reader
	if len(body) > 0 {
		requestBody = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, requestBody)
	if err != nil {
		return nil, fmt.Errorf("creating %s request for %s: %w", method, path, err)
	}

	for key, value := range headers {
		req.Header.Set(key, value)
	}

	// Ensure a reasonable default Content-Type when a payload is provided
	// without an explicit header. This avoids server-side parsing surprises
	// when tests send raw bodies.
	if len(body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("performing %s %s: %w", method, path, err)
	}

	return resp, nil
}

// TraceSimple sends a simple trace test request
func (c *TargetClient) TraceSimple(ctx context.Context, traceReq TraceRequest, opts ...TraceRequestOption) (*TraceResponse, error) {
	return c.sendTraceRequest(ctx, "/trace/simple", traceReq, opts...)
}

// TraceDownstreamHTTP sends a trace test request that triggers downstream HTTP
func (c *TargetClient) TraceDownstreamHTTP(ctx context.Context, traceReq TraceRequest, opts ...TraceRequestOption) (*TraceResponse, error) {
	return c.sendTraceRequest(ctx, "/trace/downstream-http", traceReq, opts...)
}

// TraceDownstreamGRPC sends a trace test request that triggers downstream gRPC
func (c *TargetClient) TraceDownstreamGRPC(ctx context.Context, traceReq TraceRequest, opts ...TraceRequestOption) (*TraceResponse, error) {
	return c.sendTraceRequest(ctx, "/trace/downstream-grpc", traceReq, opts...)
}

// TraceDownstreamPubSub sends a trace test request that triggers Pub/Sub propagation.
func (c *TargetClient) TraceDownstreamPubSub(ctx context.Context, traceReq TraceRequest, opts ...TraceRequestOption) (*TraceResponse, error) {
	return c.sendTraceRequest(ctx, "/trace/downstream-pubsub", traceReq, opts...)
}

// TraceParallel sends a trace test request that triggers parallel calls
func (c *TargetClient) TraceParallel(ctx context.Context, traceReq TraceRequest, opts ...TraceRequestOption) (*TraceResponse, error) {
	return c.sendTraceRequest(ctx, "/trace/parallel", traceReq, opts...)
}

// TraceNested sends a trace test request that creates nested spans
func (c *TargetClient) TraceNested(ctx context.Context, traceReq TraceRequest, opts ...TraceRequestOption) (*TraceResponse, error) {
	return c.sendTraceRequest(ctx, "/trace/nested", traceReq, opts...)
}

// TraceDownstreamGRPCServerStream invokes the downstream gRPC server-streaming trace scenario.
func (c *TargetClient) TraceDownstreamGRPCServerStream(ctx context.Context, traceReq TraceServerStreamRequest, opts ...TraceRequestOption) (*TraceResponse, error) {
	return c.sendTraceRequest(ctx, "/trace/downstream-grpc/server-stream", traceReq, opts...)
}

// TraceDownstreamGRPCClientStream exercises the downstream gRPC client-streaming trace scenario.
func (c *TargetClient) TraceDownstreamGRPCClientStream(ctx context.Context, traceReq TraceClientStreamRequest, opts ...TraceRequestOption) (*TraceResponse, error) {
	return c.sendTraceRequest(ctx, "/trace/downstream-grpc/client-stream", traceReq, opts...)
}

// TraceDownstreamGRPCBidiStream exercises the downstream gRPC bidirectional-streaming trace scenario.
func (c *TargetClient) TraceDownstreamGRPCBidiStream(ctx context.Context, traceReq TraceBidiStreamRequest, opts ...TraceRequestOption) (*TraceResponse, error) {
	return c.sendTraceRequest(ctx, "/trace/downstream-grpc/bidi-stream", traceReq, opts...)
}

// TraceRequestOption customizes outgoing trace requests.
type TraceRequestOption func(*traceHTTPRequestConfig)

type traceHTTPRequestConfig struct {
	traceID string
	spanID  string
	sampled bool
	headers map[string]string
}

// defaultTraceHTTPRequestConfig returns the default trace header configuration.
func defaultTraceHTTPRequestConfig() traceHTTPRequestConfig {
	return traceHTTPRequestConfig{
		traceID: generateTraceID(),
		spanID:  generateSpanID(),
		sampled: true,
		headers: make(map[string]string),
	}
}

// WithTraceHeader overrides the trace header applied to outgoing requests. Empty
// trace or span identifiers will be regenerated to ensure valid headers.
func WithTraceHeader(traceID, spanID string, sampled bool) TraceRequestOption {
	return func(cfg *traceHTTPRequestConfig) {
		if trimmed := strings.TrimSpace(traceID); trimmed != "" {
			cfg.traceID = trimmed
		} else {
			cfg.traceID = generateTraceID()
		}
		if trimmed := strings.TrimSpace(spanID); trimmed != "" {
			cfg.spanID = trimmed
		} else {
			cfg.spanID = generateSpanID()
		}
		cfg.sampled = sampled
	}
}

// WithAdditionalHeader injects an arbitrary HTTP header on the outgoing request.
func WithAdditionalHeader(key, value string) TraceRequestOption {
	return func(cfg *traceHTTPRequestConfig) {
		if cfg.headers == nil {
			cfg.headers = make(map[string]string)
		}
		cfg.headers[key] = value
	}
}

// sendTraceRequest is a helper to send trace test requests
func (c *TargetClient) sendTraceRequest(ctx context.Context, endpoint string, payload any, opts ...TraceRequestOption) (*TraceResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	cfg := defaultTraceHTTPRequestConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	req.Header.Set("X-Cloud-Trace-Context", buildTraceHeader(cfg.traceID, cfg.spanID, cfg.sampled))
	for key, value := range cfg.headers {
		req.Header.Set(key, value)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request returned status %d", resp.StatusCode)
	}

	var traceResp TraceResponse
	if err := json.NewDecoder(resp.Body).Decode(&traceResp); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &traceResp, nil
}

// generateTraceID returns a random W3C-compatible trace ID.
func generateTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		now := time.Now().UnixNano()
		return fmt.Sprintf("%032x", now)
	}
	return hex.EncodeToString(b[:])
}

// generateSpanID returns a random W3C-compatible span ID.
func generateSpanID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		now := time.Now().UnixNano()
		return fmt.Sprintf("%016x", now)
	}
	return hex.EncodeToString(b[:])
}

// buildTraceHeader formats an X-Cloud-Trace-Context header value.
func buildTraceHeader(traceID, spanID string, sampled bool) string {
	spanDecimal := spanID
	if parsed, err := strconv.ParseUint(spanID, 16, 64); err == nil {
		spanDecimal = strconv.FormatUint(parsed, 10)
	}
	sampleFlag := 0
	if sampled {
		sampleFlag = 1
	}
	return fmt.Sprintf("%s/%s;o=%d", traceID, spanDecimal, sampleFlag)
}
