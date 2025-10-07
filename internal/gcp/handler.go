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

package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"

	"cloud.google.com/go/logging"
	loggingpb "cloud.google.com/go/logging/apiv2/loggingpb"
)

// entryLogger defines the minimal interface for sending log entries.
type entryLogger interface {
	Log(logging.Entry)
	Flush() error
}

// groupedAttr holds an attribute along with its group context.
type groupedAttr struct {
	groups []string
	attr   slog.Attr
}

// gcpHandler formats slog.Records for Google Cloud Logging or JSON output.
type gcpHandler struct {
	mu             sync.Mutex
	cfg            Config
	entryLog       entryLogger
	redirectWriter io.Writer
	leveler        slog.Leveler

	groupedAttrs []groupedAttr
	groups       []string
}

// NewGcpHandler returns a new gcpHandler configured for Google Cloud Logging or JSON output.
func NewGcpHandler(
	cfg Config,
	gcpEntryLogger entryLogger,
	leveler slog.Leveler,
) *gcpHandler {
	if leveler == nil {
		leveler = slog.LevelInfo
	}
	h := &gcpHandler{
		cfg:            cfg,
		entryLog:       gcpEntryLogger,
		redirectWriter: cfg.RedirectWriter,
		leveler:        leveler,
		groupedAttrs:   make([]groupedAttr, 0, len(cfg.InitialAttrs)),
		groups:         make([]string, 0, 1),
	}
	if cfg.InitialGroup != "" {
		h.groups = append(h.groups, cfg.InitialGroup)
	}
	// Copy initial attributes
	copyGroups := append([]string(nil), h.groups...)
	for _, a := range cfg.InitialAttrs {
		if a.Key == "" && a.Value.Any() == nil {
			continue
		}
		h.groupedAttrs = append(h.groupedAttrs, groupedAttr{groups: copyGroups, attr: a})
	}
	return h
}

// Enabled reports whether the given log level should be handled by this handler.
// It returns true if the level is greater than or equal to the minimum level
// configured in the handler's Leveler.
func (h *gcpHandler) Enabled(_ context.Context, level slog.Level) bool {
	min := slog.LevelInfo
	if h.leveler != nil {
		min = h.leveler.Level()
	}
	return level >= min
}

// Handle processes a slog.Record and writes it to Google Cloud Logging or as JSON.
func (h *gcpHandler) Handle(ctx context.Context, r slog.Record) error {
	if !h.Enabled(ctx, r.Level) {
		return nil
	}

	// Resolve source location
	sourceLoc := h.resolveSourceLocation(r)

	// Choose project for trace formatting, preferring TraceProjectID if set.
	projectForTrace := h.cfg.ProjectID
	if h.cfg.TraceProjectID != "" {
		projectForTrace = h.cfg.TraceProjectID
	}

	// Extract trace info
	fmtTrace, rawTraceID, rawSpanID, sampled, _ := ExtractTraceSpan(ctx, projectForTrace)

	// Build payload and extract HTTP request, error, stack details, and labels
	payload, httpReq, errType, errMsg, stackStr, dynamicLabels := h.buildPayload(r)

	// Output mode
	if h.cfg.LogTarget == LogTargetGCP {
		if stackStr != "" {
			payload[stackTraceKey] = stackStr
		}
		return h.emitGCPEntry(r, payload, httpReq, sourceLoc, fmtTrace, rawSpanID, sampled, dynamicLabels)
	}
	return h.emitRedirectJSON(r, payload, httpReq, sourceLoc, fmtTrace, rawTraceID, rawSpanID, sampled, errType, errMsg, stackStr, dynamicLabels)
}

// buildPayload assembles the structured payload, extracts HTTPRequest, error type/message, stack trace, and labels.
func (h *gcpHandler) buildPayload(r slog.Record) (
	map[string]any,
	*logging.HTTPRequest,
	string, string, string,
	map[string]string,
) {
	// Capture base state
	h.mu.Lock()
	baseAttrs := append([]groupedAttr(nil), h.groupedAttrs...)
	baseGroups := append([]string(nil), h.groups...)
	h.mu.Unlock()

	payload := make(map[string]any)
	var httpReq *logging.HTTPRequest
	var firstErr error
	var stackStr string
	dynamicLabels := make(map[string]string)

	const labelsGroupName = "logging.googleapis.com/labels"

	// Recursively walk attributes, treating both handler-scoped groups and inline slog.Group(...) the same.
	var walkAttr func(ga groupedAttr)
	walkAttr = func(ga groupedAttr) {
		a := ga.attr
		if h.cfg.ReplaceAttrFunc != nil {
			a = h.cfg.ReplaceAttrFunc(ga.groups, a)
			if a.Equal(slog.Attr{}) {
				return
			}
		}

		// Descend into inline group values, appending the group's key to the current path.
		if a.Value.Kind() == slog.KindGroup {
			children := a.Value.Group()
			if len(children) == 0 {
				return
			}
			newPath := append([]string(nil), ga.groups...)
			if a.Key != "" {
				newPath = append(newPath, a.Key)
			}
			for _, child := range children {
				walkAttr(groupedAttr{groups: newPath, attr: child})
			}
			return
		}

		// Determine if the effective path is under the special labels group.
		inLabelsGroup := false
		for _, g := range ga.groups {
			if g == labelsGroupName {
				inLabelsGroup = true
				break
			}
		}

		if inLabelsGroup {
			// Labels are flat string key/value pairs.
			val := resolveSlogValue(a.Value)
			if val != nil && a.Key != "" {
				dynamicLabels[a.Key] = convertToString(val)
			}
			return
		}

		// Pull out HTTP request if present.
		if a.Key == httpRequestKey {
			if req, ok := a.Value.Any().(*logging.HTTPRequest); ok && httpReq == nil {
				httpReq = req
			}
			return
		}

		// Normal structured payload handling.
		val := resolveSlogValue(a.Value)
		if errVal, ok := val.(error); ok && firstErr == nil {
			firstErr = errVal
		}
		d := getNestedMap(payload, ga.groups)
		if a.Key != "" {
			d[a.Key] = val
		}
	}

	// Walk initial handler attrs.
	for _, ga := range baseAttrs {
		walkAttr(ga)
	}

	// Walk record attrs.
	r.Attrs(func(a slog.Attr) bool {
		walkAttr(groupedAttr{groups: baseGroups, attr: a})
		return true
	})

	// Extract error information and optional stack trace.
	errType, errMsg := "", ""
	if firstErr != nil {
		fe, origin := formatErrorForReporting(firstErr)
		errType = fe.Type
		errMsg = fe.Message
		stackStr = origin
		if stackStr == "" && h.cfg.StackTraceEnabled && r.Level >= h.cfg.StackTraceLevel {
			stackStr = captureAndFormatFallbackStack(3)
		}
	}

	return payload, httpReq, errType, errMsg, stackStr, dynamicLabels
}

// emitGCPEntry logs a structured Entry to Cloud Logging.
func (h *gcpHandler) emitGCPEntry(
	r slog.Record,
	payload map[string]any,
	httpReq *logging.HTTPRequest,
	sourceLoc *loggingpb.LogEntrySourceLocation,
	fmtTrace, spanID string,
	sampled bool,
	dynamicLabels map[string]string,
) error {
	sev := mapSlogLevelToGcpSeverity(r.Level)
	ts := r.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	if r.Message != "" {
		payload[messageKey] = r.Message
	}

	// Merge common labels with dynamic labels (dynamic takes precedence)
	var labels map[string]string
	if len(h.cfg.GCPCommonLabels) > 0 || len(dynamicLabels) > 0 {
		labels = make(map[string]string)
		// Start with common labels
		for k, v := range h.cfg.GCPCommonLabels {
			labels[k] = v
		}
		// Override with dynamic labels
		for k, v := range dynamicLabels {
			labels[k] = v
		}
	}

	// Optional request grouping via operation.
	op := ExtractOperationFromRecord(r)
	if op == nil {
		// Fallback: honor any already-materialized operation object in payload.
		op = ExtractOperationFromPayload(payload)
	}

	// Build the base entry.
	entry := logging.Entry{
		Timestamp:      ts,
		Severity:       sev,
		Payload:        payload,
		Labels:         labels,
		SourceLocation: sourceLoc,
		HTTPRequest:    httpReq,
		Operation:      op,
	}

	// If we don't have an original *http.Request (or any HTTPRequest at all),
	// populate trace fields explicitly. Otherwise, let the official client
	// auto-extract Trace/Span/Sampled from HTTPRequest.Request.
	if httpReq == nil || httpReq.Request == nil {
		entry.Trace = fmtTrace
		entry.SpanID = spanID
		entry.TraceSampled = sampled
	}

	h.entryLog.Log(entry)
	return nil
}

// emitRedirectJSON writes a JSON log line to redirectWriter.
func (h *gcpHandler) emitRedirectJSON(
	r slog.Record,
	jsonPayload map[string]any,
	httpReq *logging.HTTPRequest,
	sourceLoc *loggingpb.LogEntrySourceLocation,
	fmtTrace, rawTrace, spanID string,
	sampled bool,
	errType, errMsg, stackStr string,
	dynamicLabels map[string]string,
) error {
	if h.redirectWriter == nil {
		return errors.New("slogcp: redirect mode but no writer configured")
	}
	jsonPayload["severity"] = levelToString(r.Level)
	jsonPayload["message"] = r.Message
	jsonPayload["time"] = r.Time.UTC().Format(time.RFC3339Nano)
	if sourceLoc != nil {
		jsonPayload["logging.googleapis.com/sourceLocation"] = sourceLoc
	}
	if fmtTrace != "" {
		jsonPayload["logging.googleapis.com/trace"] = fmtTrace
		jsonPayload["logging.googleapis.com/spanId"] = spanID
		jsonPayload["logging.googleapis.com/trace_sampled"] = sampled
	} else if rawTrace != "" {
		jsonPayload["otel.trace_id"] = rawTrace
		jsonPayload["otel.span_id"] = spanID
		jsonPayload["otel.trace_sampled"] = sampled
	}
	if errType != "" {
		jsonPayload["error_type"] = errType
		jsonPayload["message"] = fmt.Sprintf("%s: %s", r.Message, errMsg)
	}
	if stackStr != "" {
		jsonPayload["stack_trace"] = stackStr
	}
	if httpReq != nil {
		if m := flattenHTTPRequestToMap(httpReq); m != nil {
			jsonPayload["httpRequest"] = m
		}
	}

	// Merge common labels with dynamic labels
	if len(h.cfg.GCPCommonLabels) > 0 || len(dynamicLabels) > 0 {
		mergedLabels := make(map[string]string)
		// Start with common labels
		for k, v := range h.cfg.GCPCommonLabels {
			mergedLabels[k] = v
		}
		// Override with dynamic labels
		for k, v := range dynamicLabels {
			mergedLabels[k] = v
		}
		jsonPayload["logging.googleapis.com/labels"] = mergedLabels
	}

	// Optional request grouping via operation, emitted under the canonical key.
	op := ExtractOperationFromRecord(r)
	if op == nil {
		// Fallback: honor any already-materialized operation object in payload.
		op = ExtractOperationFromPayload(jsonPayload)
	}
	if op != nil {
		jsonPayload["logging.googleapis.com/operation"] = map[string]any{
			"id":       op.Id,
			"producer": op.Producer,
			"first":    op.First,
			"last":     op.Last,
		}
	}

	enc := json.NewEncoder(h.redirectWriter)
	enc.SetEscapeHTML(false)
	err := enc.Encode(jsonPayload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[slogcp handler redirect] ERROR writing log entry: %v\n", err)
		return err
	}
	return nil
}

// getNestedMap navigates or creates nested maps per group path.
func getNestedMap(base map[string]any, groups []string) map[string]any {
	curr := base
	for _, g := range groups {
		if g == "" {
			continue
		}
		next, ok := curr[g]
		if m, ok2 := next.(map[string]any); ok && ok2 {
			curr = m
			continue
		}
		newMap := make(map[string]any)
		curr[g] = newMap
		curr = newMap
	}
	return curr
}

// resolveSourceLocation turns a slog.Record into a LogEntrySourceLocation.
func (h *gcpHandler) resolveSourceLocation(r slog.Record) *loggingpb.LogEntrySourceLocation {
	if !h.cfg.AddSource || r.PC == 0 {
		return nil
	}

	frames := runtime.CallersFrames([]uintptr{r.PC})
	frame, _ := frames.Next() // more is irrelevant for a single frame

	if frame.Function == "" { // defensive: runtime couldn't resolve symbol
		return nil
	}

	return &loggingpb.LogEntrySourceLocation{
		File:     frame.File,
		Line:     int64(frame.Line),
		Function: frame.Function,
	}
}

// WithAttrs returns a new handler with additional attributes.
func (h *gcpHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h2 := h.cloneLocked()
	copyGroups := append([]string(nil), h2.groups...)
	for _, a := range attrs {
		if a.Key == "" && a.Value.Any() == nil {
			continue
		}
		h2.groupedAttrs = append(h2.groupedAttrs, groupedAttr{groups: copyGroups, attr: a})
	}
	return h2
}

// WithGroup returns a new handler with nested group.
func (h *gcpHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h2 := h.cloneLocked()
	h2.groups = append(h2.groups, name)
	return h2
}

// cloneLocked duplicates handler state; caller must hold h.mu.
func (h *gcpHandler) cloneLocked() *gcpHandler {
	h2 := &gcpHandler{
		cfg:            h.cfg,
		entryLog:       h.entryLog,
		redirectWriter: h.redirectWriter,
		leveler:        h.leveler,
		groupedAttrs:   append([]groupedAttr(nil), h.groupedAttrs...),
		groups:         append([]string(nil), h.groups...),
	}
	return h2
}

// captureAndFormatFallbackStack captures a stack trace for fallback.
func captureAndFormatFallbackStack(skip int) string {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(skip+1, pcs)
	if n == 0 {
		return ""
	}
	return formatPCsToStackString(pcs[:n])
}

// Ensure gcpHandler implements slog.Handler.
var _ slog.Handler = (*gcpHandler)(nil)
