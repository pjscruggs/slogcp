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

package slogcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type groupedAttr struct {
	groups []string
	attr   slog.Attr
}

// extractErrorFromValue unwraps an error from a slog.Value when possible.
func extractErrorFromValue(v slog.Value) error {
	v = v.Resolve()
	if v.Kind() != slog.KindAny {
		return nil
	}
	if anyVal := v.Any(); anyVal != nil {
		if err, ok := anyVal.(error); ok {
			return err
		}
	}
	return nil
}

type payloadState struct {
	root       map[string]any
	labels     map[string]string
	freeMaps   []map[string]any
	usedMaps   []map[string]any
	groupStack []string
}

// prepare resets and returns the root payload map sized according to the
// provided capacity hint.
func (ps *payloadState) prepare(capacity int) map[string]any {
	if capacity < 4 {
		capacity = 4
	}
	if ps.root == nil {
		ps.root = make(map[string]any, capacity)
	} else {
		clear(ps.root)
	}
	ps.usedMaps = ps.usedMaps[:0]
	return ps.root
}

// borrowMap retrieves a scratch map from the pool sized with the supplied
// hint, allocating a fresh map when necessary.
func (ps *payloadState) borrowMap(hint int) map[string]any {
	if hint < 4 {
		hint = 4
	}
	var m map[string]any
	if n := len(ps.freeMaps); n > 0 {
		m = ps.freeMaps[n-1]
		ps.freeMaps = ps.freeMaps[:n-1]
		clear(m)
	} else {
		m = make(map[string]any, hint)
	}
	ps.usedMaps = append(ps.usedMaps, m)
	return m
}

// obtainLabels returns a map suitable for labels, clearing any previous
// contents.
func (ps *payloadState) obtainLabels() map[string]string {
	if ps.labels == nil {
		ps.labels = make(map[string]string, 4)
	} else {
		clear(ps.labels)
	}
	return ps.labels
}

// recycle returns pooled maps and slices to their zero state for reuse.
func (ps *payloadState) recycle() {
	for _, m := range ps.usedMaps {
		clear(m)
		ps.freeMaps = append(ps.freeMaps, m)
	}
	ps.usedMaps = ps.usedMaps[:0]
	if ps.root != nil {
		clear(ps.root)
	}
	if ps.labels != nil {
		clear(ps.labels)
	}
	ps.groupStack = ps.groupStack[:0]
}

var payloadStatePool = sync.Pool{
	New: func() any {
		return &payloadState{}
	},
}

var jsonBufferPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

var (
	recordSourceFunc     atomic.Value // func(slog.Record) *slog.Source
	runtimeFrameResolver atomic.Value // func(uintptr) runtime.Frame
)

// init seeds hook defaults for record source and frame resolution.
func init() {
	setRecordSourceFunc(func(r slog.Record) *slog.Source {
		return r.Source()
	})
	setRuntimeFrameResolver(func(pc uintptr) runtime.Frame {
		var pcs [1]uintptr
		pcs[0] = pc
		frames := runtime.CallersFrames(pcs[:])
		frame, _ := frames.Next()
		return frame
	})
}

// setRecordSourceFunc installs the hook used to obtain source metadata from records.
func setRecordSourceFunc(fn func(slog.Record) *slog.Source) {
	if fn == nil {
		fn = func(r slog.Record) *slog.Source { return r.Source() }
	}
	recordSourceFunc.Store(fn)
}

// getRecordSourceFunc returns the configured record source hook.
func getRecordSourceFunc() func(slog.Record) *slog.Source {
	if fn, ok := recordSourceFunc.Load().(func(slog.Record) *slog.Source); ok && fn != nil {
		return fn
	}
	fn := func(r slog.Record) *slog.Source { return r.Source() }
	recordSourceFunc.Store(fn)
	return fn
}

// setRuntimeFrameResolver installs the hook used to resolve call frames.
func setRuntimeFrameResolver(fn func(uintptr) runtime.Frame) {
	if fn == nil {
		fn = func(pc uintptr) runtime.Frame {
			var pcs [1]uintptr
			pcs[0] = pc
			frames := runtime.CallersFrames(pcs[:])
			frame, _ := frames.Next()
			return frame
		}
	}
	runtimeFrameResolver.Store(fn)
}

// getRuntimeFrameResolver returns the configured call frame resolver hook.
func getRuntimeFrameResolver() func(uintptr) runtime.Frame {
	if fn, ok := runtimeFrameResolver.Load().(func(uintptr) runtime.Frame); ok && fn != nil {
		return fn
	}
	fn := func(pc uintptr) runtime.Frame {
		var pcs [1]uintptr
		pcs[0] = pc
		frames := runtime.CallersFrames(pcs[:])
		frame, _ := frames.Next()
		return frame
	}
	runtimeFrameResolver.Store(fn)
	return fn
}

type sourceLocation struct {
	File     string `json:"file"`
	Line     int64  `json:"line"`
	Function string `json:"function"`
}

type jsonHandler struct {
	mu *sync.Mutex

	cfg            *handlerConfig
	leveler        slog.Leveler
	writer         io.Writer
	internalLogger *slog.Logger
	bufferPool     *sync.Pool

	groupedAttrs []groupedAttr
	groups       []string
}

// newJSONHandler constructs a handler that renders slog records as Google
// Cloud Logging compatible JSON.
func newJSONHandler(cfg *handlerConfig, leveler slog.Leveler, internalLogger *slog.Logger) *jsonHandler {
	if leveler == nil {
		leveler = slog.LevelInfo
	}
	h := &jsonHandler{
		mu:             &sync.Mutex{},
		cfg:            cfg,
		leveler:        leveler,
		writer:         cfg.Writer,
		internalLogger: internalLogger,
		bufferPool:     &jsonBufferPool,
		groupedAttrs:   make([]groupedAttr, 0, initialGroupedCapacity(cfg)),
		groups:         make([]string, 0, len(cfg.InitialGroups)+1),
	}
	h.appendInitialGroups(cfg.InitialGroups)
	h.groupedAttrs = append(h.groupedAttrs, h.collectInitialAttrs(cfg)...)
	return h
}

// initialGroupedCapacity estimates groupedAttr allocation capacity.
func initialGroupedCapacity(cfg *handlerConfig) int {
	if len(cfg.InitialGroupedAttrs) > 0 {
		return len(cfg.InitialGroupedAttrs)
	}
	return len(cfg.InitialAttrs)
}

// appendInitialGroups seeds the handler with configured groups.
func (h *jsonHandler) appendInitialGroups(groups []string) {
	if len(groups) > 0 {
		h.groups = append(h.groups, groups...)
	}
}

// collectInitialAttrs clones initial attrs into groupedAttr form.
func (h *jsonHandler) collectInitialAttrs(cfg *handlerConfig) []groupedAttr {
	if len(cfg.InitialGroupedAttrs) > 0 {
		return h.cloneGroupedAttrs(cfg.InitialGroupedAttrs)
	}
	return h.cloneUngroupedAttrs(cfg.InitialAttrs)
}

// cloneGroupedAttrs copies grouped attributes while skipping empty entries.
func (h *jsonHandler) cloneGroupedAttrs(initial []groupedAttr) []groupedAttr {
	var grouped []groupedAttr
	for _, ga := range initial {
		if ga.attr.Key == "" && ga.attr.Value.Any() == nil {
			continue
		}
		grouped = append(grouped, groupedAttr{
			groups: append([]string(nil), ga.groups...),
			attr:   ga.attr,
		})
	}
	return grouped
}

// cloneUngroupedAttrs wraps ungrouped attrs with the current groups.
func (h *jsonHandler) cloneUngroupedAttrs(initial []slog.Attr) []groupedAttr {
	var grouped []groupedAttr
	copyGroups := append([]string(nil), h.groups...)
	for _, a := range initial {
		if a.Key == "" && a.Value.Any() == nil {
			continue
		}
		grouped = append(grouped, groupedAttr{groups: copyGroups, attr: a})
	}
	return grouped
}

// Enabled reports whether level is enabled for emission.
func (h *jsonHandler) Enabled(_ context.Context, level slog.Level) bool {
	minLevel := slog.LevelInfo
	if h.leveler != nil {
		minLevel = h.leveler.Level()
	}
	return level >= minLevel
}

// Handle serializes r into the Cloud Logging wire format, enriching it with
// trace context and runtime metadata before writing to the configured writer.
//
// Example:
//
//	logger := slog.New(h)
//	logger.Info("index", slog.Int("docs", 42))
func (h *jsonHandler) Handle(ctx context.Context, r slog.Record) error {
	if !h.Enabled(ctx, r.Level) {
		return nil
	}

	sourceLoc := h.resolveSourceLocation(r)

	projectForTrace := h.cfg.TraceProjectID
	fmtTrace, rawTraceID, rawSpanID, sampled, spanCtx := ExtractTraceSpan(ctx, projectForTrace)
	ownsSpan := spanCtx.IsValid() && !spanCtx.IsRemote()

	state := payloadStatePool.Get().(*payloadState)
	defer func() {
		state.recycle()
		payloadStatePool.Put(state)
	}()

	payload, httpReq, errType, errMsg, stackStr, dynamicLabels := h.buildPayload(r, state)

	if len(dynamicLabels) > 0 {
		payload[labelsGroupKey] = dynamicLabels
	}

	return h.emitJSON(r, payload, httpReq, sourceLoc, fmtTrace, rawTraceID, rawSpanID, ownsSpan, sampled, errType, errMsg, stackStr)
}

// WithAttrs returns a new handler that includes the provided attributes on
// every emitted record.
func (h *jsonHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}

	h.mu.Lock()
	baseGrouped := append([]groupedAttr(nil), h.groupedAttrs...)
	baseGroups := append([]string(nil), h.groups...)
	cfg := h.cfg
	leveler := h.leveler
	writer := h.writer
	internalLogger := h.internalLogger
	h.mu.Unlock()

	grouped := baseGrouped
	for _, attr := range attrs {
		grouped = append(grouped, groupedAttr{
			groups: append([]string(nil), baseGroups...),
			attr:   attr,
		})
	}

	return &jsonHandler{
		mu:             h.mu,
		cfg:            cfg,
		leveler:        leveler,
		writer:         writer,
		internalLogger: internalLogger,
		bufferPool:     h.bufferPool,
		groupedAttrs:   grouped,
		groups:         baseGroups,
	}
}

// WithGroup nests subsequent attributes under name.
func (h *jsonHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	h.mu.Lock()
	baseGrouped := append([]groupedAttr(nil), h.groupedAttrs...)
	baseGroups := append([]string(nil), h.groups...)
	cfg := h.cfg
	leveler := h.leveler
	writer := h.writer
	internalLogger := h.internalLogger
	h.mu.Unlock()

	baseGroups = append(baseGroups, name)

	return &jsonHandler{
		mu:             h.mu,
		cfg:            cfg,
		leveler:        leveler,
		writer:         writer,
		internalLogger: internalLogger,
		bufferPool:     h.bufferPool,
		groupedAttrs:   baseGrouped,
		groups:         baseGroups,
	}
}

// resolveSourceLocation determines the best-effort source location for r when
// source logging is enabled.
func (h *jsonHandler) resolveSourceLocation(r slog.Record) *sourceLocation {
	if !h.cfg.AddSource {
		return nil
	}

	if src := getRecordSourceFunc()(r); src != nil {
		if src.Function != "" || src.File != "" {
			return &sourceLocation{
				File:     src.File,
				Line:     int64(src.Line),
				Function: src.Function,
			}
		}
	}

	if r.PC == 0 {
		return nil
	}

	frame := getRuntimeFrameResolver()(r.PC)
	if frame.Function == "" {
		return nil
	}
	return &sourceLocation{
		File:     frame.File,
		Line:     int64(frame.Line),
		Function: frame.Function,
	}
}

// buildPayload flattens r into a JSON-ready map alongside optional HTTP
// request metadata, error context, stack traces, and dynamic labels.
func (h *jsonHandler) buildPayload(r slog.Record, state *payloadState) (
	map[string]any,
	*HTTPRequest,
	string, string, string,
	map[string]string,
) {
	baseAttrs, baseGroups := h.snapshotBaseState()
	builder := newPayloadBuilder(h, state, r, baseAttrs, baseGroups)
	return builder.build()
}

// snapshotBaseState captures grouped attributes and groups under lock.
func (h *jsonHandler) snapshotBaseState() ([]groupedAttr, []string) {
	h.mu.Lock()
	baseAttrs := h.groupedAttrs
	baseGroups := h.groups
	h.mu.Unlock()
	return baseAttrs, baseGroups
}

type payloadBuilder struct {
	handler       *jsonHandler
	state         *payloadState
	record        slog.Record
	baseAttrs     []groupedAttr
	baseGroups    []string
	payload       map[string]any
	httpReq       *HTTPRequest
	firstErr      error
	stackStr      string
	dynamicLabels map[string]string
	groupStack    []string
}

// newPayloadBuilder constructs a builder for the current record.
func newPayloadBuilder(h *jsonHandler, state *payloadState, r slog.Record, baseAttrs []groupedAttr, baseGroups []string) *payloadBuilder {
	return &payloadBuilder{
		handler:    h,
		state:      state,
		record:     r,
		baseAttrs:  baseAttrs,
		baseGroups: baseGroups,
		groupStack: state.groupStack[:0],
	}
}

// build assembles the payload, HTTP request, and error metadata.
func (pb *payloadBuilder) build() (map[string]any, *HTTPRequest, string, string, string, map[string]string) {
	pb.preparePayload()
	pb.applyBaseAttrs()
	pb.applyRecordAttrs()
	errType, errMsg := pb.resolveErrorAndStack()
	pb.state.groupStack = pb.groupStack[:0]
	pruneEmptyMaps(pb.payload)
	return pb.payload, pb.httpReq, errType, errMsg, pb.stackStr, pb.dynamicLabels
}

// preparePayload allocates the root payload map with a capacity hint.
func (pb *payloadBuilder) preparePayload() {
	estimatedFields := max(len(pb.baseAttrs)+int(pb.record.NumAttrs()), 4)
	pb.payload = pb.state.prepare(estimatedFields)
}

// applyBaseAttrs walks initial grouped attributes into the payload.
func (pb *payloadBuilder) applyBaseAttrs() {
	for i := range pb.baseAttrs {
		ga := pb.baseAttrs[i]
		curr := pb.payload
		inLabels := false
		pb.groupStack = pb.groupStack[:0]
		for _, g := range ga.groups {
			pb.groupStack = append(pb.groupStack, g)
			if g == LabelsGroup {
				inLabels = true
				continue
			}
			curr = pb.ensureGroupMap(curr, g, 4)
		}
		pb.walkAttr(len(pb.groupStack), curr, ga.attr, inLabels)
	}
}

// applyRecordAttrs walks record attributes using any base group context.
func (pb *payloadBuilder) applyRecordAttrs() {
	pb.groupStack = pb.groupStack[:0]
	baseMap := pb.payload
	baseInLabels := false
	for _, g := range pb.baseGroups {
		pb.groupStack = append(pb.groupStack, g)
		if g == LabelsGroup {
			baseInLabels = true
			continue
		}
		baseMap = pb.ensureGroupMap(baseMap, g, 4)
	}
	baseGroupsLen := len(pb.groupStack)

	pb.record.Attrs(func(attr slog.Attr) bool {
		pb.walkAttr(baseGroupsLen, baseMap, attr, baseInLabels)
		return true
	})
}

// walkAttr processes a single attribute, dispatching to group or leaf handlers.
func (pb *payloadBuilder) walkAttr(groupsLen int, currMap map[string]any, attr slog.Attr, inLabels bool) {
	attr, rawValue, kind := pb.normalizeAttr(groupsLen, attr)

	if attr.Key == "" {
		return
	}

	if kind == slog.KindGroup {
		pb.walkGroupAttr(groupsLen, currMap, attr, inLabels)
		return
	}

	pb.handleLeafAttr(currMap, attr, rawValue, inLabels)
}

// normalizeAttr applies ReplaceAttr and resolves the attribute kind.
func (pb *payloadBuilder) normalizeAttr(groupsLen int, attr slog.Attr) (slog.Attr, slog.Value, slog.Kind) {
	rawValue := attr.Value
	attr.Value = attr.Value.Resolve()
	kind := attr.Value.Kind()
	if kind != slog.KindGroup && pb.handler.cfg.ReplaceAttr != nil {
		var groupsArg []string
		if groupsLen > 0 {
			groupsArg = pb.groupStack[:groupsLen]
		}
		attr = pb.handler.cfg.ReplaceAttr(groupsArg, attr)
		rawValue = attr.Value
		attr.Value = attr.Value.Resolve()
		kind = attr.Value.Kind()
	}
	if kind != slog.KindGroup {
		pb.captureFirstError(attr.Value)
	}
	return attr, rawValue, kind
}

// walkGroupAttr descends into grouped attributes while tracking labels and groups.
func (pb *payloadBuilder) walkGroupAttr(groupsLen int, currMap map[string]any, attr slog.Attr, inLabels bool) {
	children := attr.Value.Group()
	nextGroupsLen := groupsLen
	nextMap := currMap
	appended := false
	if attr.Key != "" {
		pb.groupStack = append(pb.groupStack, attr.Key)
		appended = true
		nextGroupsLen++
		if attr.Key != LabelsGroup {
			nextMap = pb.ensureGroupMap(currMap, attr.Key, len(children))
		}
	}
	nextInLabels := inLabels || attr.Key == LabelsGroup
	for i := range children {
		pb.walkAttr(nextGroupsLen, nextMap, children[i], nextInLabels)
	}
	if appended {
		pb.groupStack = pb.groupStack[:groupsLen]
	}
}

// handleLeafAttr writes non-group attributes to the payload or labels.
func (pb *payloadBuilder) handleLeafAttr(currMap map[string]any, attr slog.Attr, rawValue slog.Value, inLabels bool) {
	if inLabels {
		pb.addLabel(attr)
		return
	}
	if attr.Key == httpRequestKey && pb.handleHTTPRequestAttr(rawValue) {
		return
	}
	if val := resolveSlogValue(attr.Value); val != nil {
		currMap[attr.Key] = val
	}
}

// addLabel inserts a label value when it converts successfully.
func (pb *payloadBuilder) addLabel(attr slog.Attr) {
	if s, ok := labelValueToString(attr.Value); ok {
		pb.ensureLabels()[attr.Key] = s
	}
}

// handleHTTPRequestAttr extracts HTTP request payloads from attribute values.
func (pb *payloadBuilder) handleHTTPRequestAttr(rawValue slog.Value) bool {
	if pb.httpReq != nil {
		return false
	}
	if req, ok := httpRequestFromValue(rawValue); ok {
		PrepareHTTPRequest(req)
		pb.httpReq = req
		return true
	}
	return false
}

// ensureLabels allocates the dynamic labels map when needed.
func (pb *payloadBuilder) ensureLabels() map[string]string {
	if pb.dynamicLabels == nil {
		pb.dynamicLabels = pb.state.obtainLabels()
	}
	return pb.dynamicLabels
}

// ensureGroupMap creates or reuses nested maps for grouped attributes.
func (pb *payloadBuilder) ensureGroupMap(parent map[string]any, key string, hint int) map[string]any {
	if key == "" {
		return parent
	}
	if existing, ok := parent[key]; ok {
		if m, ok := existing.(map[string]any); ok {
			return m
		}
	}
	child := pb.state.borrowMap(hint)
	parent[key] = child
	return child
}

// captureFirstError records the first encountered error value.
func (pb *payloadBuilder) captureFirstError(val slog.Value) {
	if pb.firstErr != nil {
		return
	}
	if errVal := extractErrorFromValue(val); errVal != nil {
		pb.firstErr = errVal
	}
}

// resolveErrorAndStack determines error metadata and stack traces.
func (pb *payloadBuilder) resolveErrorAndStack() (string, string) {
	if pb.firstErr != nil {
		fe, origin := formatErrorForReporting(pb.firstErr)
		pb.stackStr = origin
		if pb.stackStr == "" && pb.shouldCaptureStack() {
			pb.stackStr = captureAndFormatFallbackStack()
		}
		return fe.Type, fe.Message
	}
	if pb.shouldCaptureStack() {
		if stack, _ := CaptureStack(nil); stack != "" {
			pb.stackStr = stack
		}
	}
	return "", ""
}

// shouldCaptureStack determines whether stack collection should occur.
func (pb *payloadBuilder) shouldCaptureStack() bool {
	return pb.handler.cfg.StackTraceEnabled &&
		stackTraceComparisonLevel(pb.record.Level) >= stackTraceComparisonLevel(pb.handler.cfg.StackTraceLevel)
}

// stackTraceComparisonLevel normalizes levels for stack trace thresholds so
// LevelDefault does not outrank error/critical levels.
func stackTraceComparisonLevel(level slog.Level) slog.Level {
	if level == slog.Level(LevelDefault) {
		return slog.LevelInfo
	}
	return level
}

// emitJSON writes the fully constructed Cloud Logging payload to the handler
// writer.
func (h *jsonHandler) emitJSON(
	r slog.Record,
	jsonPayload map[string]any,
	httpReq *HTTPRequest,
	sourceLoc *sourceLocation,
	fmtTrace, rawTrace, spanID string,
	ownsSpan bool,
	sampled bool,
	errType, errMsg, stackStr string,
) error {
	if h.writer == nil {
		return errors.New("slogcp: no writer configured")
	}

	h.applySeverityAndTime(jsonPayload, r)
	h.applySourceLocation(jsonPayload, sourceLoc)
	h.applyTrace(jsonPayload, fmtTrace, rawTrace, spanID, ownsSpan, sampled)
	message := h.applyErrorFields(jsonPayload, r.Message, errType, errMsg, stackStr)
	jsonPayload[messageKey] = message
	h.applyServiceContext(jsonPayload)
	h.applyHTTPRequest(jsonPayload, httpReq)

	return h.writeJSONPayload(jsonPayload)
}

// applySeverityAndTime sets severity and timestamp fields on the payload.
func (h *jsonHandler) applySeverityAndTime(jsonPayload map[string]any, r slog.Record) {
	jsonPayload["severity"] = severityString(r.Level, h.cfg.UseShortSeverityNames)
	if h.cfg.EmitTimeField {
		jsonPayload["time"] = r.Time.UTC().Format(time.RFC3339Nano)
	}
}

// applySourceLocation attaches source metadata when available.
func (h *jsonHandler) applySourceLocation(jsonPayload map[string]any, sourceLoc *sourceLocation) {
	if sourceLoc != nil {
		jsonPayload["logging.googleapis.com/sourceLocation"] = sourceLoc
	}
}

// applyTrace writes trace identifiers based on formatting preferences.
func (h *jsonHandler) applyTrace(jsonPayload map[string]any, fmtTrace, rawTrace, spanID string, ownsSpan bool, sampled bool) {
	switch {
	case fmtTrace != "":
		h.applyCloudTrace(jsonPayload, fmtTrace, spanID, ownsSpan, sampled)
	case rawTrace != "" && h.cfg.traceAllowAutoformat:
		h.applyCloudTrace(jsonPayload, rawTrace, spanID, ownsSpan, sampled)
	case rawTrace != "":
		h.applyUnknownProjectTrace(jsonPayload, rawTrace, spanID, sampled)
	}
}

// applyCloudTrace writes Cloud Trace correlation fields.
func (h *jsonHandler) applyCloudTrace(jsonPayload map[string]any, traceID, spanID string, ownsSpan bool, sampled bool) {
	jsonPayload[TraceKey] = traceID
	if ownsSpan && spanID != "" {
		jsonPayload[SpanKey] = spanID
	}
	jsonPayload[SampledKey] = sampled
}

// applyUnknownProjectTrace emits OpenTelemetry identifiers when project is unknown.
func (h *jsonHandler) applyUnknownProjectTrace(jsonPayload map[string]any, rawTrace, spanID string, sampled bool) {
	if h.cfg.traceDiagnosticsState != nil {
		h.cfg.traceDiagnosticsState.warnUnknownProject()
	}
	jsonPayload["otel.trace_id"] = rawTrace
	jsonPayload["otel.span_id"] = spanID
	jsonPayload["otel.trace_sampled"] = sampled
}

// applyErrorFields adjusts message and stack outputs based on error metadata.
func (h *jsonHandler) applyErrorFields(jsonPayload map[string]any, message string, errType, errMsg, stackStr string) string {
	if errType != "" {
		jsonPayload["error_type"] = errType
		message = message + ": " + errMsg
	}
	if stackStr != "" {
		jsonPayload[stackTraceKey] = stackStr
	}
	return message
}

// applyServiceContext attaches runtime service context when unset.
func (h *jsonHandler) applyServiceContext(jsonPayload map[string]any) {
	if len(h.cfg.runtimeServiceContext) == 0 {
		return
	}
	if _, exists := jsonPayload["serviceContext"]; exists {
		return
	}
	if h.cfg.runtimeServiceContextAny != nil {
		jsonPayload["serviceContext"] = h.cfg.runtimeServiceContextAny
		return
	}
	jsonPayload["serviceContext"] = stringMapToAny(h.cfg.runtimeServiceContext)
}

// applyHTTPRequest flattens HTTP requests into the payload when present.
func (h *jsonHandler) applyHTTPRequest(jsonPayload map[string]any, httpReq *HTTPRequest) {
	if httpReq == nil {
		return
	}
	if m := flattenHTTPRequestToMap(httpReq); m != nil {
		jsonPayload[httpRequestKey] = m
	}
}

// writeJSONPayload encodes and writes the log entry to the configured writer.
func (h *jsonHandler) writeJSONPayload(jsonPayload map[string]any) error {
	var (
		buf    *bytes.Buffer
		pooled bool
	)
	if h.bufferPool != nil {
		pooled = true
		buf = h.bufferPool.Get().(*bytes.Buffer)
	} else {
		buf = &bytes.Buffer{}
	}
	buf.Reset()
	defer func() {
		if pooled {
			buf.Reset()
			h.bufferPool.Put(buf)
		}
	}()

	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(jsonPayload); err != nil {
		h.internalLogger.Error("failed to render JSON log entry", slog.Any("error", err))
		return fmt.Errorf("encode JSON payload: %w", err)
	}

	h.mu.Lock()
	_, err := buf.WriteTo(h.writer)
	h.mu.Unlock()
	if err != nil {
		h.internalLogger.Error("failed to write JSON log entry", slog.Any("error", err))
		return fmt.Errorf("write JSON payload: %w", err)
	}
	return nil
}

// captureAndFormatFallbackStack captures the current goroutine stack as a
// formatted string.
func captureAndFormatFallbackStack() string {
	stack, _ := CaptureStack(nil)
	return stack
}

// pruneEmptyMaps recursively removes map entries that are empty, ensuring that
// WithGroup-derived handlers do not emit empty objects when no attributes are
// present for a group.
func pruneEmptyMaps(m map[string]any) bool {
	for k, v := range m {
		switch typed := v.(type) {
		case map[string]any:
			if pruneEmptyMaps(typed) {
				delete(m, k)
			}
		case map[string]string:
			if len(typed) == 0 {
				delete(m, k)
			}
		}
	}
	return len(m) == 0
}
