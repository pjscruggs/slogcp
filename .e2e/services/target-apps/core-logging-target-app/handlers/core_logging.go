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

// Package handlers contains HTTP handlers used by the core logging target app.
package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/pjscruggs/slogcp"
	"github.com/pjscruggs/slogcp/slogcphttp"
)

// LogRequest represents the structure of a log request
type LogRequest struct {
	Message    string            `json:"message"`
	TestID     string            `json:"test_id"`
	Attributes map[string]any    `json:"attributes,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Operation  *Operation        `json:"operation,omitempty"`
}

// Operation represents Cloud Logging operation grouping metadata.
type Operation struct {
	ID       string `json:"id"`
	Producer string `json:"producer,omitempty"`
	First    bool   `json:"first,omitempty"`
	Last     bool   `json:"last,omitempty"`
}

// CoreLoggingHandler handles requests for testing core logging functionality
type CoreLoggingHandler struct {
	logger                *slog.Logger
	defaultSeverityLogger *slog.Logger
}

// NewCoreLoggingHandler creates a new handler with the provided logger
func NewCoreLoggingHandler(logger *slog.Logger, defaultSeverityLogger *slog.Logger) *CoreLoggingHandler {
	if defaultSeverityLogger == nil {
		defaultSeverityLogger = logger
	}
	return &CoreLoggingHandler{logger: logger, defaultSeverityLogger: defaultSeverityLogger}
}

// Common response structure
type logResponse struct {
	Success   bool              `json:"success"`
	Message   string            `json:"message"`
	Severity  string            `json:"severity"`
	Timestamp string            `json:"timestamp"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// LogDefault handles DEFAULT severity logging
func (h *CoreLoggingHandler) LogDefault(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogDefault request", "error", err)
		return
	}

	// GCP removes the severity field entirely when the emitted severity is DEFAULT (or unset),
	// but queries for severity=DEFAULT still match those entries.
	// Emit an info-level log that should be filtered out by the high minimum level logger.
	h.defaultSeverityLogger.InfoContext(ctx, "DEFAULT severity preflight info",
		"test_id", logReq.TestID,
		"endpoint", "/log/default",
		"method", r.Method,
		"filtered", true,
	)

	slogcp.DefaultContext(ctx, h.defaultSeverityLogger, logReq.Message,
		"test_id", logReq.TestID,
		"endpoint", "/log/default",
		"method", r.Method,
	)

	respondJSON(w, logResponse{
		Success:   true,
		Message:   logReq.Message,
		Severity:  "DEFAULT",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// LogDebug handles DEBUG severity logging
func (h *CoreLoggingHandler) LogDebug(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogDebug request", "error", err)
		return
	}

	h.logger.DebugContext(ctx, logReq.Message,
		"test_id", logReq.TestID,
		"endpoint", "/log/debug",
		"method", r.Method,
	)

	respondJSON(w, logResponse{
		Success:   true,
		Message:   logReq.Message,
		Severity:  "INFO",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// LogInfo handles INFO severity logging
func (h *CoreLoggingHandler) LogInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogInfo request", "error", err)
		return
	}

	h.logger.InfoContext(ctx, logReq.Message,
		"test_id", logReq.TestID,
		"endpoint", "/log/info",
		"method", r.Method,
	)

	respondJSON(w, logResponse{
		Success:   true,
		Message:   logReq.Message,
		Severity:  "INFO",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// LogNotice handles NOTICE severity logging
func (h *CoreLoggingHandler) LogNotice(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogNotice request", "error", err)
		return
	}

	slogcp.NoticeContext(ctx, h.logger, logReq.Message,
		"test_id", logReq.TestID,
		"endpoint", "/log/notice",
		"method", r.Method,
	)

	respondJSON(w, logResponse{
		Success:   true,
		Message:   logReq.Message,
		Severity:  "INFO",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// LogWarning handles WARNING severity logging
func (h *CoreLoggingHandler) LogWarning(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogWarning request", "error", err)
		return
	}

	h.logger.WarnContext(ctx, logReq.Message,
		"test_id", logReq.TestID,
		"endpoint", "/log/warning",
		"method", r.Method,
	)

	respondJSON(w, logResponse{
		Success:   true,
		Message:   logReq.Message,
		Severity:  "INFO",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// LogError handles ERROR severity logging
func (h *CoreLoggingHandler) LogError(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogError request", "error", err)
		return
	}

	testErr := fmt.Errorf("test error for severity ERROR")

	h.logger.ErrorContext(ctx, logReq.Message,
		"test_id", logReq.TestID,
		"endpoint", "/log/error",
		"method", r.Method,
		"error", testErr,
	)

	respondJSON(w, logResponse{
		Success:   true,
		Message:   logReq.Message,
		Severity:  "INFO",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// LogCritical handles CRITICAL severity logging
func (h *CoreLoggingHandler) LogCritical(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogCritical request", "error", err)
		return
	}

	slogcp.CriticalContext(ctx, h.logger, logReq.Message,
		"test_id", logReq.TestID,
		"endpoint", "/log/critical",
		"method", r.Method,
	)

	respondJSON(w, logResponse{
		Success:   true,
		Message:   logReq.Message,
		Severity:  "INFO",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// LogAlert handles ALERT severity logging
func (h *CoreLoggingHandler) LogAlert(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogAlert request", "error", err)
		return
	}

	slogcp.AlertContext(ctx, h.logger, logReq.Message,
		"test_id", logReq.TestID,
		"endpoint", "/log/alert",
		"method", r.Method,
	)

	respondJSON(w, logResponse{
		Success:   true,
		Message:   logReq.Message,
		Severity:  "INFO",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// LogEmergency handles EMERGENCY severity logging
func (h *CoreLoggingHandler) LogEmergency(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogEmergency request", "error", err)
		return
	}

	slogcp.EmergencyContext(ctx, h.logger, logReq.Message,
		"test_id", logReq.TestID,
		"endpoint", "/log/emergency",
		"method", r.Method,
	)

	respondJSON(w, logResponse{
		Success:   true,
		Message:   logReq.Message,
		Severity:  "INFO",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// LogStructured handles structured payload logging
func (h *CoreLoggingHandler) LogStructured(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogStructured request", "error", err)
		return
	}

	h.logger.InfoContext(ctx, logReq.Message,
		slog.String("test_id", logReq.TestID),
		slog.String("endpoint", "/log/structured"),
		slog.Any("attributes", logReq.Attributes),
	)

	respondJSON(w, logResponse{
		Success:   true,
		Message:   "Structured log created",
		Severity:  "INFO",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Metadata: map[string]string{
			"type": "structured",
		},
	})
}

// LogNested handles deeply nested structured logging
func (h *CoreLoggingHandler) LogNested(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogNested request", "error", err)
		return
	}

	h.logger.InfoContext(ctx, logReq.Message,
		slog.String("test_id", logReq.TestID),
		slog.String("endpoint", "/log/nested"),
		slog.Any("attributes", logReq.Attributes),
	)

	respondJSON(w, logResponse{
		Success:   true,
		Message:   "Nested log created",
		Severity:  "INFO",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Metadata: map[string]string{
			"type": "nested",
		},
	})
}

// LogWithOperation emits a log entry that includes Cloud Logging operation grouping metadata.
func (h *CoreLoggingHandler) LogWithOperation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogWithOperation request", "error", err)
		return
	}

	if logReq.Operation == nil {
		http.Error(w, "Missing 'operation' in request body", http.StatusBadRequest)
		h.logger.WarnContext(ctx, "Operation grouping request missing operation data",
			"test_id", logReq.TestID,
			"endpoint", "/log/operation",
		)
		return
	}

	opID := logReq.Operation.ID
	if opID == "" {
		opID = logReq.TestID
	}

	h.logger.InfoContext(ctx, logReq.Message,
		slog.String("test_id", logReq.TestID),
		slog.String("endpoint", "/log/operation"),
		slog.Group("logging.googleapis.com/operation",
			slog.String("id", opID),
			slog.String("producer", logReq.Operation.Producer),
			slog.Bool("first", logReq.Operation.First),
			slog.Bool("last", logReq.Operation.Last),
		),
	)

	respondJSON(w, logResponse{
		Success:   true,
		Message:   "Log with operation metadata created",
		Severity:  "INFO",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Metadata: map[string]string{
			"operation_id": opID,
		},
	})
}

// LogWithLabels handles logging with custom labels
func (h *CoreLoggingHandler) LogWithLabels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogWithLabels request", "error", err)
		return
	}

	// Include any dynamic labels provided by the request which should override common labels
	labelAttrs := make([]any, 0, len(logReq.Labels))
	for key, value := range logReq.Labels {
		labelAttrs = append(labelAttrs, slog.String(key, value))
	}

	attrs := []any{
		slog.String("test_id", logReq.TestID),
		slog.String("endpoint", "/log/labels"),
	}

	if len(labelAttrs) > 0 {
		attrs = append(attrs, slog.Group(slogcp.LabelsGroup, labelAttrs...))
	}

	h.logger.InfoContext(ctx, logReq.Message, attrs...)

	respondJSON(w, logResponse{
		Success:   true,
		Message:   "Log with labels created",
		Severity:  "INFO",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Metadata: map[string]string{
			"type": "labels",
		},
	})
}

// LogHTTPRequest emits a log with a Cloud Logging-compatible httpRequest payload built from the live request.
func (h *CoreLoggingHandler) LogHTTPRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogHTTPRequest request", "error", err)
		return
	}

	scope, _ := slogcphttp.ScopeFromContext(ctx)

	respBody := logResponse{
		Success:   true,
		Message:   logReq.Message,
		Severity:  "INFO",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Metadata: map[string]string{
			"endpoint": "/log/http-request",
			"kind":     "manual_http_request_attr",
		},
	}

	encoded, err := json.Marshal(respBody)
	if err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		h.logger.ErrorContext(ctx, "Failed to marshal http request response", "error", err)
		return
	}

	status := http.StatusOK
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(encoded); err != nil {
		h.logger.ErrorContext(ctx, "Failed to write http request response", "error", err)
		return
	}

	httpReq := buildHTTPRequestPayload(r, scope, status, int64(len(encoded)))

	h.logger.InfoContext(ctx, logReq.Message,
		slog.String("test_id", logReq.TestID),
		slog.String("endpoint", "/log/http-request"),
		slog.String("http_request_kind", "manual"),
		slog.Any("httpRequest", httpReq),
	)
}

// LogHTTPRequestInflight emits a log with httpRequest metadata captured before the response completes.
func (h *CoreLoggingHandler) LogHTTPRequestInflight(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogHTTPRequestInflight request", "error", err)
		return
	}

	scope, _ := slogcphttp.ScopeFromContext(ctx)

	// Emit the log before writing the response so httpRequest reflects an in-flight request.
	httpAttr := slogcphttp.HTTPRequestAttr(r, scope)
	h.logger.InfoContext(ctx, logReq.Message,
		slog.String("test_id", logReq.TestID),
		slog.String("endpoint", "/log/http-request-inflight"),
		slog.String("http_request_kind", "inflight"),
		httpAttr,
	)

	respondJSON(w, logResponse{
		Success:   true,
		Message:   logReq.Message,
		Severity:  "INFO",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Metadata: map[string]string{
			"endpoint": "/log/http-request-inflight",
			"kind":     "inflight_http_request_attr",
		},
	})
}

// LogHTTPRequestParserResidual emits a log with known + unknown httpRequest
// fields to probe backend promotion and residual payload behavior.
func (h *CoreLoggingHandler) LogHTTPRequestParserResidual(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogHTTPRequestParserResidual request", "error", err)
		return
	}

	scope, _ := slogcphttp.ScopeFromContext(ctx)
	requestURL := requestURLForLogging(r, scope)

	h.logger.InfoContext(ctx, logReq.Message,
		slog.String("test_id", logReq.TestID),
		slog.String("endpoint", "/log/http-request-parser-residual"),
		slog.String("http_request_kind", "parser_residual_probe"),
		slog.Any("httpRequest", map[string]any{
			"requestMethod": "GET",
			"requestUrl":    requestURL,
			"status":        204,
			"customField":   "keep-me",
			"anotherCustom": 42,
		}),
	)

	respondJSON(w, logResponse{
		Success:   true,
		Message:   logReq.Message,
		Severity:  "INFO",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Metadata: map[string]string{
			"endpoint": "/log/http-request-parser-residual",
			"kind":     "parser_residual_probe",
		},
	})
}

// LogHTTPRequestParserStatusZero emits a log with status=0 to probe backend
// status omission behavior in promoted httpRequest metadata.
func (h *CoreLoggingHandler) LogHTTPRequestParserStatusZero(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogHTTPRequestParserStatusZero request", "error", err)
		return
	}

	scope, _ := slogcphttp.ScopeFromContext(ctx)
	requestURL := requestURLForLogging(r, scope)

	h.logger.InfoContext(ctx, logReq.Message,
		slog.String("test_id", logReq.TestID),
		slog.String("endpoint", "/log/http-request-parser-status-zero"),
		slog.String("http_request_kind", "parser_status_zero_probe"),
		slog.Any("httpRequest", map[string]any{
			"requestMethod": "GET",
			"requestUrl":    requestURL,
			"status":        0,
		}),
	)

	respondJSON(w, logResponse{
		Success:   true,
		Message:   logReq.Message,
		Severity:  "INFO",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Metadata: map[string]string{
			"endpoint": "/log/http-request-parser-status-zero",
			"kind":     "parser_status_zero_probe",
		},
	})
}

// LogBatch handles batch logging across multiple severities
func (h *CoreLoggingHandler) LogBatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logReq, err := decodeLogRequest(w, r)
	if err != nil {
		h.logger.WarnContext(ctx, "Failed to decode LogBatch request", "error", err)
		return
	}

	batchSize := 5
	if size, ok := logReq.Attributes["batch_size"].(float64); ok {
		batchSize = int(size)
	}

	for i := 0; i < batchSize; i++ {
		h.logger.InfoContext(ctx, fmt.Sprintf("Batch log %d of %d", i+1, batchSize),
			"test_id", logReq.TestID, // Use the same testID for all batch entries
			"batch_index", i+1,
		)
		time.Sleep(10 * time.Millisecond)
	}

	respondJSON(w, logResponse{
		Success:   true,
		Message:   fmt.Sprintf("Batch of %d logs created", batchSize),
		Severity:  "INFO",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Metadata: map[string]string{
			"batch_id": logReq.TestID,
			"count":    fmt.Sprintf("%d", batchSize),
		},
	})
}

// MiddlewareEcho exercises slogcp HTTP middleware body/header capture paths.
func (h *CoreLoggingHandler) MiddlewareEcho(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	defer func() {
		if err := r.Body.Close(); err != nil {
			h.logger.WarnContext(ctx, "Failed to close middleware echo request body", "error", err)
		}
	}()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		h.logger.ErrorContext(ctx, "Failed to read middleware echo body", "error", err)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("X-Test-Response", "echoed")

	if _, err := fmt.Fprintf(w, "echo:%s", string(body)); err != nil {
		h.logger.ErrorContext(ctx, "Failed to write middleware echo response", "error", err)
	}
}

// MiddlewarePanic intentionally panics so middleware recovery paths can be validated.
func (h *CoreLoggingHandler) MiddlewarePanic(_ http.ResponseWriter, r *http.Request) {
	testID := r.URL.Query().Get("test_id")
	panic(fmt.Sprintf("intentional middleware panic for %s", testID))
}

// MiddlewareSkip exercises middleware skip-path logic.
func (h *CoreLoggingHandler) MiddlewareSkip(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprintf(w, "skip middleware logging path reached for %s", r.URL.Path); err != nil {
		h.logger.ErrorContext(r.Context(), "Failed to write middleware skip response", "error", err)
	}
}

// buildHTTPRequestPayload constructs a Cloud Logging httpRequest payload using the live RequestScope.
func buildHTTPRequestPayload(r *http.Request, scope *slogcphttp.RequestScope, status int, responseSize int64) *slogcp.HTTPRequest {
	req := slogcp.HTTPRequestFromRequest(r)
	if req == nil {
		req = &slogcp.HTTPRequest{}
	}
	applyScopeToHTTPRequest(req, scope)
	if status > 0 {
		req.Status = status
	}
	if responseSize > 0 {
		req.ResponseSize = responseSize
	}
	slogcp.PrepareHTTPRequest(req)
	return req
}

// applyScopeToHTTPRequest overlays request metadata extracted from scope.
func applyScopeToHTTPRequest(req *slogcp.HTTPRequest, scope *slogcphttp.RequestScope) {
	if req == nil || scope == nil {
		return
	}
	applyScopeIdentity(req, scope)
	applyScopeSizing(req, scope)
	applyScopeStatusAndLatency(req, scope)
}

// applyScopeIdentity overlays method, URL, and client identity from scope.
func applyScopeIdentity(req *slogcp.HTTPRequest, scope *slogcphttp.RequestScope) {
	if method := scope.Method(); method != "" {
		req.RequestMethod = method
	}
	if url := buildRequestURLFromScope(scope); url != "" {
		req.RequestURL = url
	}
	if req.RemoteIP == "" {
		req.RemoteIP = scope.ClientIP()
	}
	if req.UserAgent == "" {
		req.UserAgent = scope.UserAgent()
	}
}

// applyScopeSizing overlays request and response sizes from scope.
func applyScopeSizing(req *slogcp.HTTPRequest, scope *slogcphttp.RequestScope) {
	if req.RequestSize == 0 {
		req.RequestSize = scope.RequestSize()
	}
	if req.ResponseSize == 0 {
		req.ResponseSize = scope.ResponseSize()
	}
}

// applyScopeStatusAndLatency overlays status and latency from scope.
func applyScopeStatusAndLatency(req *slogcp.HTTPRequest, scope *slogcphttp.RequestScope) {
	if req.Status == 0 {
		req.Status = scope.Status()
	}
	if latency := time.Since(scope.Start()); latency >= 0 {
		req.Latency = latency
	}
}

// buildRequestURLFromScope reconstructs a request URL from scoped fields when available.
func buildRequestURLFromScope(scope *slogcphttp.RequestScope) string {
	if scope == nil {
		return ""
	}

	scheme := strings.TrimSpace(scope.Scheme())
	host := strings.TrimSpace(scope.Host())
	target := scope.Target()
	query := scope.Query()

	var b strings.Builder
	if scheme != "" && host != "" {
		b.WriteString(scheme)
		b.WriteString("://")
		b.WriteString(host)
	}
	if target != "" {
		b.WriteString(target)
	}
	if query != "" {
		b.WriteByte('?')
		b.WriteString(query)
	}
	return b.String()
}

// requestURLForLogging returns a best-effort absolute URL for httpRequest logs.
func requestURLForLogging(r *http.Request, scope *slogcphttp.RequestScope) string {
	if url := buildRequestURLFromScope(scope); url != "" {
		return url
	}
	if r == nil || r.URL == nil {
		return ""
	}
	if r.URL.IsAbs() {
		return r.URL.String()
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if r.Host != "" {
		return fmt.Sprintf("%s://%s%s", scheme, r.Host, r.URL.String())
	}
	return r.URL.String()
}

// respondJSON sends a JSON 200 OK response with the provided data.
func respondJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// decodeLogRequest decodes the JSON body of a log request
func decodeLogRequest(w http.ResponseWriter, r *http.Request) (*LogRequest, error) {
	var logReq LogRequest
	defer func() {
		if err := r.Body.Close(); err != nil {
			slog.Warn("Failed to close request body", "error", err)
		}
	}()
	if err := json.NewDecoder(r.Body).Decode(&logReq); err != nil {
		http.Error(w, "Failed to decode request body", http.StatusBadRequest)
		return nil, fmt.Errorf("decode log request body: %w", err)
	}

	if logReq.TestID == "" {
		http.Error(w, "Missing 'test_id' in request body", http.StatusBadRequest)
		return nil, fmt.Errorf("missing 'test_id' in request body")
	}
	return &logReq, nil
}
