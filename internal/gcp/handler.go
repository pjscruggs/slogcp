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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"cloud.google.com/go/logging"
	loggingpb "cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/types/known/structpb"
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

type payloadState struct {
	root     map[string]any
	labels   map[string]string
	freeMaps []map[string]any
	usedMaps []map[string]any
}

func (ps *payloadState) prepare(capacity int) map[string]any {
	if capacity < 4 {
		capacity = 4
	}
	if ps.root == nil {
		ps.root = make(map[string]any, capacity)
	} else {
		clearMapAny(ps.root)
	}
	ps.usedMaps = ps.usedMaps[:0]
	return ps.root
}

func (ps *payloadState) borrowMap(hint int) map[string]any {
	if hint < 4 {
		hint = 4
	}
	var m map[string]any
	if n := len(ps.freeMaps); n > 0 {
		m = ps.freeMaps[n-1]
		ps.freeMaps = ps.freeMaps[:n-1]
		clearMapAny(m)
	} else {
		m = make(map[string]any, hint)
	}
	ps.usedMaps = append(ps.usedMaps, m)
	return m
}

func (ps *payloadState) obtainLabels() map[string]string {
	if ps.labels == nil {
		ps.labels = make(map[string]string, 4)
	} else {
		clearStringMap(ps.labels)
	}
	return ps.labels
}

func (ps *payloadState) recycle() {
	for _, m := range ps.usedMaps {
		clearMapAny(m)
		ps.freeMaps = append(ps.freeMaps, m)
	}
	ps.usedMaps = ps.usedMaps[:0]
	if ps.root != nil {
		clearMapAny(ps.root)
	}
	if ps.labels != nil {
		clearStringMap(ps.labels)
	}
}

var payloadStatePool = sync.Pool{
	New: func() any {
		return &payloadState{}
	},
}

func clearMapAny(m map[string]any) {
	for k := range m {
		delete(m, k)
	}
}

func clearStringMap(m map[string]string) {
	for k := range m {
		delete(m, k)
	}
}

var labelStringBufferPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

var pcBufferPool = sync.Pool{
	New: func() any {
		buf := make([]uintptr, 64)
		return &buf
	},
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

	if h.cfg.LogTarget == LogTargetGCP {
		payload, protoPayload, httpReq, _, _, stackStr, dynamicLabels := h.buildPayload(r, true, nil)
		if stackStr != "" {
			payload[stackTraceKey] = stackStr
			if protoPayload != nil {
				if protoPayload.Fields == nil {
					protoPayload.Fields = make(map[string]*structpb.Value, 4)
				}
				protoPayload.Fields[stackTraceKey] = structpb.NewStringValue(stackStr)
			}
		}
		return h.emitGCPEntry(r, payload, protoPayload, httpReq, sourceLoc, fmtTrace, rawSpanID, sampled, dynamicLabels)
	}

	state := payloadStatePool.Get().(*payloadState)
	defer func() {
		state.recycle()
		payloadStatePool.Put(state)
	}()

	payload, _, httpReq, errType, errMsg, stackStr, dynamicLabels := h.buildPayload(r, false, state)

	var mergedLabels map[string]string
	if len(h.cfg.GCPCommonLabels) > 0 || len(dynamicLabels) > 0 {
		if dynamicLabels != nil {
			mergedLabels = dynamicLabels
		} else {
			mergedLabels = state.obtainLabels()
		}
		for k, v := range h.cfg.GCPCommonLabels {
			if _, exists := mergedLabels[k]; !exists {
				mergedLabels[k] = v
			}
		}
		if len(mergedLabels) > 0 {
			payload["logging.googleapis.com/labels"] = mergedLabels
		}
	}

	return h.emitRedirectJSON(r, payload, httpReq, sourceLoc, fmtTrace, rawTraceID, rawSpanID, sampled, errType, errMsg, stackStr, mergedLabels)
}

// buildPayload assembles the structured payload, extracts HTTPRequest, error type/message, stack trace, and labels.
func (h *gcpHandler) buildPayload(r slog.Record, buildProto bool, state *payloadState) (
	map[string]any,
	*structpb.Struct,
	*logging.HTTPRequest,
	string, string, string,
	map[string]string,
) {
	// Capture base state
	h.mu.Lock()
	baseAttrs := append([]groupedAttr(nil), h.groupedAttrs...)
	baseGroups := append([]string(nil), h.groups...)
	h.mu.Unlock()

	estimatedFields := len(baseAttrs) + int(r.NumAttrs())
	if estimatedFields < 4 {
		estimatedFields = 4
	}
	var payload map[string]any
	if state != nil {
		payload = state.prepare(estimatedFields)
	} else {
		payload = make(map[string]any, estimatedFields)
	}
	var httpReq *logging.HTTPRequest
	var firstErr error
	var stackStr string
	var dynamicLabels map[string]string
	var protoRoot *structpb.Struct
	protoSuccess := buildProto
	if buildProto {
		protoRoot = &structpb.Struct{Fields: make(map[string]*structpb.Value, estimatedFields)}
	}

	const labelsGroupName = "logging.googleapis.com/labels"

	ensureLabels := func() map[string]string {
		if dynamicLabels == nil {
			if state != nil {
				dynamicLabels = state.obtainLabels()
			} else {
				dynamicLabels = make(map[string]string, 4)
			}
		}
		return dynamicLabels
	}

	ensureGroupMap := func(parent map[string]any, key string, hint int) map[string]any {
		if key == "" {
			return parent
		}
		if existing, ok := parent[key]; ok {
			if m, ok2 := existing.(map[string]any); ok2 {
				return m
			}
		}
		var child map[string]any
		if state != nil {
			child = state.borrowMap(hint)
		} else {
			if hint <= 0 {
				hint = 4
			}
			child = make(map[string]any, hint)
		}
		parent[key] = child
		return child
	}

	ensureProtoStruct := func(parent *structpb.Struct, key string, hint int) *structpb.Struct {
		if parent == nil {
			return nil
		}
		if parent.Fields == nil {
			if hint <= 0 {
				hint = 4
			}
			parent.Fields = make(map[string]*structpb.Value, hint)
		}
		if existing, ok := parent.Fields[key]; ok {
			if s := existing.GetStructValue(); s != nil {
				return s
			}
		}
		if hint <= 0 {
			hint = 4
		}
		child := &structpb.Struct{Fields: make(map[string]*structpb.Value, hint)}
		parent.Fields[key] = structpb.NewStructValue(child)
		return child
	}

	groupStack := make([]string, 0, len(baseGroups)+4)

	loadGroups := func(groups []string) (int, map[string]any, *structpb.Struct) {
		groupStack = append(groupStack[:0], groups...)

		curr := payload
		currProto := protoRoot
		for _, g := range groups {
			if g != "" && g != labelsGroupName {
				curr = ensureGroupMap(curr, g, 0)
				if protoSuccess {
					currProto = ensureProtoStruct(currProto, g, 0)
				}
			}
		}
		return len(groupStack), curr, currProto
	}

	// walkAttr recursively processes handler and record attributes, preserving the current
	// group context and map pointer so nested maps are only materialized when needed.
	var walkAttr func(depth int, currMap map[string]any, currProto *structpb.Struct, a slog.Attr, inLabels bool)
	walkAttr = func(depth int, currMap map[string]any, currProto *structpb.Struct, a slog.Attr, inLabels bool) {
		groupsView := groupStack[:depth]
		if h.cfg.ReplaceAttrFunc != nil {
			a = h.cfg.ReplaceAttrFunc(groupsView, a)
			if a.Equal(slog.Attr{}) {
				return
			}
		}

		// Descend into inline slog.Group values, extending the current group stack and
		// reusing an existing child map for non-label groups where possible.
		if a.Value.Kind() == slog.KindGroup {
			children := a.Value.Group()
			if len(children) == 0 {
				return
			}
			nextDepth := depth
			appended := false
			nextMap := currMap
			nextProto := currProto
			if a.Key != "" {
				if len(groupStack) == nextDepth {
					groupStack = append(groupStack, a.Key)
				} else {
					groupStack[nextDepth] = a.Key
					groupStack = groupStack[:nextDepth+1]
				}
				if a.Key != labelsGroupName {
					nextMap = ensureGroupMap(currMap, a.Key, len(children))
					if protoSuccess {
						nextProto = ensureProtoStruct(currProto, a.Key, len(children))
					}
				}
				nextDepth++
				appended = true
			} else {
				nextMap = currMap
			}
			nextInLabels := inLabels
			if !nextInLabels && a.Key == labelsGroupName {
				nextInLabels = true
			}
			for i := range children {
				walkAttr(nextDepth, nextMap, nextProto, children[i], nextInLabels)
			}
			if appended {
				groupStack = groupStack[:depth]
			}
			return
		}

		// Attributes inside the labels group are converted to flat string key/value pairs.
		if inLabels {
			// Labels are surfaced as flat string key/value pairs.
			if a.Key == "" {
				return
			}
			if s, ok := labelValueToString(a.Value); ok {
				ensureLabels()[a.Key] = s
			}
			return
		}

		// Pull out HTTP request attributes early so they can hydrate the structured entry.
		if a.Key == httpRequestKey {
			if req, ok := a.Value.Any().(*logging.HTTPRequest); ok && httpReq == nil {
				httpReq = req
			}
			return
		}

		// Normal structured payload handling.
		val := resolveSlogValue(a.Value)
		if val == nil {
			return
		}
		if errVal, ok := val.(error); ok && firstErr == nil {
			firstErr = errVal
		}
		if a.Key == "" {
			return
		}
		currMap[a.Key] = val

		if protoSuccess && currProto != nil {
			if currProto.Fields == nil {
				currProto.Fields = make(map[string]*structpb.Value, 4)
			}
			var protoVal *structpb.Value
			if v, ok := slogValueToProto(a.Value); ok {
				protoVal = v
			} else if v, ok := anyToProtoValue(val, 0); ok {
				protoVal = v
			} else {
				protoSuccess = false
				protoRoot = nil
				return
			}
			currProto.Fields[a.Key] = protoVal
		}
	}

	// Walk initial handler attributes.
	for i := range baseAttrs {
		ga := baseAttrs[i]
		depth, currMap, currProto := loadGroups(ga.groups)
		walkAttr(depth, currMap, currProto, ga.attr, containsGroup(ga.groups, labelsGroupName))
	}

	// Walk the record attributes with the handler's base group context.
	baseDepth, baseMap, baseProto := loadGroups(baseGroups)
	baseInLabels := containsGroup(baseGroups, labelsGroupName)
	r.Attrs(func(a slog.Attr) bool {
		walkAttr(baseDepth, baseMap, baseProto, a, baseInLabels)
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
			stackStr = captureAndFormatFallbackStack()
		}
	}

	if !protoSuccess {
		protoRoot = nil
	}
	return payload, protoRoot, httpReq, errType, errMsg, stackStr, dynamicLabels
}

// emitGCPEntry logs a structured Entry to Cloud Logging.
func (h *gcpHandler) emitGCPEntry(
	r slog.Record,
	payload map[string]any,
	protoPayload *structpb.Struct,
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
		if protoPayload != nil {
			if protoPayload.Fields == nil {
				protoPayload.Fields = make(map[string]*structpb.Value, 4)
			}
			protoPayload.Fields[messageKey] = structpb.NewStringValue(r.Message)
		}
	}

	// Merge common labels with dynamic labels (dynamic takes precedence)
	var labels map[string]string
	switch {
	case len(dynamicLabels) == 0:
		if len(h.cfg.GCPCommonLabels) > 0 {
			labels = h.cfg.GCPCommonLabels
		}
	case len(h.cfg.GCPCommonLabels) == 0:
		labels = dynamicLabels
	default:
		labels = make(map[string]string, len(h.cfg.GCPCommonLabels)+len(dynamicLabels))
		for k, v := range h.cfg.GCPCommonLabels {
			labels[k] = v
		}
		for k, v := range dynamicLabels {
			labels[k] = v
		}
	}

	if len(h.cfg.GCPCommonLabels) > 0 || len(dynamicLabels) > 0 {
		mergedLabels := make(map[string]string, len(h.cfg.GCPCommonLabels)+len(dynamicLabels))
		for k, v := range h.cfg.GCPCommonLabels {
			mergedLabels[k] = v
		}
		for k, v := range dynamicLabels {
			mergedLabels[k] = v
		}
		payload["logging.googleapis.com/labels"] = mergedLabels
		if protoPayload != nil {
			if protoPayload.Fields == nil {
				protoPayload.Fields = make(map[string]*structpb.Value, 4)
			}
			labelStruct := &structpb.Struct{Fields: make(map[string]*structpb.Value, len(mergedLabels))}
			for k, v := range mergedLabels {
				labelStruct.Fields[k] = structpb.NewStringValue(v)
			}
			protoPayload.Fields["logging.googleapis.com/labels"] = structpb.NewStructValue(labelStruct)
		}
	}

	// Optional request grouping via operation.
	op := ExtractOperationFromRecord(r)
	if op == nil {
		// Fallback: honor any already-materialized operation object in payload.
		op = ExtractOperationFromPayload(payload)
	}

	entryPayload := any(payload)
	if protoPayload != nil {
		entryPayload = protoPayload
	}

	// Build the base entry.
	entry := logging.Entry{
		Timestamp:      ts,
		Severity:       sev,
		Payload:        entryPayload,
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

	// Merge common labels with dynamic labels (skip if already populated upstream)
	if _, exists := jsonPayload["logging.googleapis.com/labels"]; !exists {
		switch {
		case len(dynamicLabels) == 0:
			if len(h.cfg.GCPCommonLabels) > 0 {
				jsonPayload["logging.googleapis.com/labels"] = h.cfg.GCPCommonLabels
			}
		case len(h.cfg.GCPCommonLabels) == 0:
			jsonPayload["logging.googleapis.com/labels"] = dynamicLabels
		default:
			mergedLabels := make(map[string]string, len(h.cfg.GCPCommonLabels)+len(dynamicLabels))
			for k, v := range h.cfg.GCPCommonLabels {
				mergedLabels[k] = v
			}
			for k, v := range dynamicLabels {
				mergedLabels[k] = v
			}
			jsonPayload["logging.googleapis.com/labels"] = mergedLabels
		}
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

func containsGroup(groups []string, target string) bool {
	for i := range groups {
		if groups[i] == target {
			return true
		}
	}
	return false
}

// labelValueToString converts a slog.Value into its string form suitable for Cloud Logging label
// emission, returning false when the value cannot be represented as a string label.
func labelValueToString(v slog.Value) (string, bool) {
	rv := v.Resolve()
	switch rv.Kind() {
	case slog.KindString:
		return rv.String(), true
	case slog.KindInt64:
		return strconv.FormatInt(rv.Int64(), 10), true
	case slog.KindUint64:
		return strconv.FormatUint(rv.Uint64(), 10), true
	case slog.KindFloat64:
		return strconv.FormatFloat(rv.Float64(), 'g', -1, 64), true
	case slog.KindBool:
		if rv.Bool() {
			return "true", true
		}
		return "false", true
	case slog.KindDuration:
		return rv.Duration().String(), true
	case slog.KindTime:
		return rv.Time().Format(time.RFC3339), true
	case slog.KindAny:
		val := rv.Any()
		if val == nil {
			return "", false
		}
		switch x := val.(type) {
		case time.Duration:
			return x.String(), true
		case time.Time:
			return x.Format(time.RFC3339), true
		case string:
			return x, true
		case []byte:
			return string(x), true
		case fmt.Stringer:
			return x.String(), true
		case error:
			return x.Error(), true
		}
		buf := labelStringBufferPool.Get().(*bytes.Buffer)
		buf.Reset()
		fmt.Fprint(buf, val)
		s := buf.String()
		buf.Reset()
		labelStringBufferPool.Put(buf)
		return s, true
	}
	return "", false
}

// resolveSourceLocation turns a slog.Record into a LogEntrySourceLocation.
func (h *gcpHandler) resolveSourceLocation(r slog.Record) *loggingpb.LogEntrySourceLocation {
	if !h.cfg.AddSource || r.PC == 0 {
		return nil
	}

	// TODO: After cloud.google.com/go/logging officially moves to Go 1.25, prefer
	// the Source-aware fast path below to reuse compiler-provided file,
	// function, and line information instead of walking CallersFrames. That lets
	// us preserve upstream source rewriting and avoid the extra runtime work.
	//
	//	if src := r.Source(); src != nil {
	//		if src.File != "" || src.Function != "" || src.Line != 0 {
	//			return &loggingpb.LogEntrySourceLocation{
	//				File:     src.File,
	//				Line:     int64(src.Line),
	//				Function: src.Function,
	//			}
	//		}
	//	}

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
func captureAndFormatFallbackStack() string {
	bufPtr := pcBufferPool.Get().(*[]uintptr)
	pcs := (*bufPtr)[:cap(*bufPtr)]
	n := runtime.Callers(0, pcs)
	if n == 0 {
		pcBufferPool.Put(bufPtr)
		return ""
	}
	trimmed := trimFallbackStackPCs(pcs[:n])
	if len(trimmed) == 0 {
		trimmed = pcs[:n]
	}
	stack := formatPCsToStackString(trimmed)
	pcBufferPool.Put(bufPtr)
	return stack
}

// Ensure gcpHandler implements slog.Handler.
var _ slog.Handler = (*gcpHandler)(nil)
