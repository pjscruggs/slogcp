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
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/pjscruggs/slogcp/chatter"
)

type groupedAttr struct {
	groups []string
	attr   slog.Attr
}

type payloadState struct {
	root       map[string]any
	labels     map[string]string
	freeMaps   []map[string]any
	usedMaps   []map[string]any
	groupStack []string
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
	ps.groupStack = ps.groupStack[:0]
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

type sourceLocation struct {
	File     string `json:"file"`
	Line     int64  `json:"line"`
	Function string `json:"function"`
}

type jsonHandler struct {
	mu sync.Mutex

	cfg            *handlerConfig
	leveler        slog.Leveler
	writer         io.Writer
	internalLogger *slog.Logger

	groupedAttrs []groupedAttr
	groups       []string
}

func newJSONHandler(cfg *handlerConfig, leveler slog.Leveler, internalLogger *slog.Logger) *jsonHandler {
	if leveler == nil {
		leveler = slog.LevelInfo
	}
	h := &jsonHandler{
		cfg:            cfg,
		leveler:        leveler,
		writer:         cfg.Writer,
		internalLogger: internalLogger,
		groupedAttrs:   make([]groupedAttr, 0, len(cfg.InitialAttrs)),
		groups:         make([]string, 0, 1),
	}
	if cfg.InitialGroup != "" {
		h.groups = append(h.groups, cfg.InitialGroup)
	}
	copyGroups := append([]string(nil), h.groups...)
	for _, a := range cfg.InitialAttrs {
		if a.Key == "" && a.Value.Any() == nil {
			continue
		}
		h.groupedAttrs = append(h.groupedAttrs, groupedAttr{groups: copyGroups, attr: a})
	}
	return h
}

func (h *jsonHandler) Enabled(_ context.Context, level slog.Level) bool {
	min := slog.LevelInfo
	if h.leveler != nil {
		min = h.leveler.Level()
	}
	return level >= min
}

func (h *jsonHandler) Handle(ctx context.Context, r slog.Record) error {
	if decision, ok := chatter.DecisionFromContext(ctx); ok && decision != nil {
		if decision.ShouldDrop() {
			return nil
		}
		switch strings.ToUpper(decision.SeverityHint()) {
		case "WARN":
			if r.Level < slog.LevelWarn {
				r.Level = slog.LevelWarn
			}
		case "ERROR":
			if r.Level < slog.LevelError {
				r.Level = slog.LevelError
			}
		}
	}

	if !h.Enabled(ctx, r.Level) {
		return nil
	}

	sourceLoc := h.resolveSourceLocation(r)

	projectForTrace := h.cfg.TraceProjectID
	fmtTrace, rawTraceID, rawSpanID, sampled, _ := ExtractTraceSpan(ctx, projectForTrace)

	state := payloadStatePool.Get().(*payloadState)
	defer func() {
		state.recycle()
		payloadStatePool.Put(state)
	}()

	payload, httpReq, errType, errMsg, stackStr, dynamicLabels := h.buildPayload(r, state)

	finalLabels := mergeStringMaps(h.cfg.runtimeLabels, dynamicLabels)
	if len(finalLabels) > 0 {
		payload[labelsGroupKey] = finalLabels
	}

	return h.emitJSON(r, payload, httpReq, sourceLoc, fmtTrace, rawTraceID, rawSpanID, sampled, errType, errMsg, stackStr)
}

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
		cfg:            cfg,
		leveler:        leveler,
		writer:         writer,
		internalLogger: internalLogger,
		groupedAttrs:   grouped,
		groups:         baseGroups,
	}
}

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
		cfg:            cfg,
		leveler:        leveler,
		writer:         writer,
		internalLogger: internalLogger,
		groupedAttrs:   baseGrouped,
		groups:         baseGroups,
	}
}

func (h *jsonHandler) resolveSourceLocation(r slog.Record) *sourceLocation {
	if !h.cfg.AddSource {
		return nil
	}

	if src := r.Source(); src != nil {
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

	var pcs [1]uintptr
	pcs[0] = r.PC
	frames := runtime.CallersFrames(pcs[:])
	frame, _ := frames.Next()
	if frame.Function == "" {
		return nil
	}
	return &sourceLocation{
		File:     frame.File,
		Line:     int64(frame.Line),
		Function: frame.Function,
	}
}

func (h *jsonHandler) buildPayload(r slog.Record, state *payloadState) (
	map[string]any,
	*HTTPRequest,
	string, string, string,
	map[string]string,
) {
	h.mu.Lock()
	baseAttrs := append([]groupedAttr(nil), h.groupedAttrs...)
	baseGroups := append([]string(nil), h.groups...)
	h.mu.Unlock()

	estimatedFields := len(baseAttrs) + int(r.NumAttrs())
	if estimatedFields < 4 {
		estimatedFields = 4
	}
	payload := state.prepare(estimatedFields)

	var httpReq *HTTPRequest
	var firstErr error
	var stackStr string
	var dynamicLabels map[string]string

	ensureLabels := func() map[string]string {
		if dynamicLabels == nil {
			dynamicLabels = state.obtainLabels()
		}
		return dynamicLabels
	}

	ensureGroupMap := func(parent map[string]any, key string, hint int) map[string]any {
		if key == "" {
			return parent
		}
		if existing, ok := parent[key]; ok {
			if m, ok := existing.(map[string]any); ok {
				return m
			}
		}
		child := state.borrowMap(hint)
		parent[key] = child
		return child
	}

	groupStack := state.groupStack[:0]
	const labelsGroupName = LabelsGroup

	var walkAttr func(depth int, currMap map[string]any, attr slog.Attr, inLabels bool)
	walkAttr = func(depth int, currMap map[string]any, attr slog.Attr, inLabels bool) {
		attr.Value = attr.Value.Resolve()

		if attr.Value.Kind() == slog.KindGroup {
			children := attr.Value.Group()
			nextDepth := depth
			nextMap := currMap
			appended := false
			if attr.Key != "" {
				if len(groupStack) == nextDepth {
					groupStack = append(groupStack, attr.Key)
				} else {
					if nextDepth < len(groupStack) {
						groupStack[nextDepth] = attr.Key
						groupStack = groupStack[:nextDepth+1]
					} else {
						groupStack = append(groupStack, attr.Key)
					}
				}
				if attr.Key != labelsGroupName {
					nextMap = ensureGroupMap(currMap, attr.Key, len(children))
				}
				nextDepth++
				appended = true
			}
			nextInLabels := inLabels || attr.Key == labelsGroupName
			for i := range children {
				walkAttr(nextDepth, nextMap, children[i], nextInLabels)
			}
			if appended {
				groupStack = groupStack[:depth]
			}
			return
		}

		if inLabels {
			if attr.Key == "" {
				return
			}
			if s, ok := labelValueToString(attr.Value); ok {
				ensureLabels()[attr.Key] = s
			}
			return
		}

		if attr.Key == httpRequestKey {
			if req, ok := attr.Value.Any().(*HTTPRequest); ok && httpReq == nil {
				PrepareHTTPRequest(req)
				httpReq = req
			}
			return
		}

		val := resolveSlogValue(attr.Value)
		if val == nil || attr.Key == "" {
			return
		}
		if errVal, ok := val.(error); ok && firstErr == nil {
			firstErr = errVal
		}
		currMap[attr.Key] = val
	}

	loadBaseGroups := func() (int, map[string]any, bool) {
		depth := 0
		curr := payload
		inLabels := false
		for _, g := range baseGroups {
			if g == labelsGroupName {
				inLabels = true
				continue
			}
			curr = ensureGroupMap(curr, g, 4)
			if len(groupStack) == depth {
				groupStack = append(groupStack, g)
			} else {
				groupStack[depth] = g
			}
			depth++
		}
		return depth, curr, inLabels
	}

	for i := range baseAttrs {
		ga := baseAttrs[i]
		attr := ga.attr
		if h.cfg.ReplaceAttr != nil {
			attr = h.cfg.ReplaceAttr(ga.groups, attr)
		}
		depth := 0
		curr := payload
		inLabels := false
		for _, g := range ga.groups {
			if g == labelsGroupName {
				inLabels = true
				continue
			}
			curr = ensureGroupMap(curr, g, 4)
			depth++
		}
		walkAttr(depth, curr, attr, inLabels)
	}

	baseDepth, baseMap, baseInLabels := loadBaseGroups()
	r.Attrs(func(attr slog.Attr) bool {
		walkAttr(baseDepth, baseMap, attr, baseInLabels)
		return true
	})

	errType, errMsg := "", ""
	if firstErr != nil {
		fe, origin := formatErrorForReporting(firstErr)
		errType = fe.Type
		errMsg = fe.Message
		stackStr = origin
		if stackStr == "" && h.cfg.StackTraceEnabled && r.Level >= h.cfg.StackTraceLevel {
			stackStr = captureAndFormatFallbackStack()
		}
	} else if h.cfg.StackTraceEnabled && r.Level >= h.cfg.StackTraceLevel {
		if stack, _ := CaptureStack(nil); stack != "" {
			stackStr = stack
		}
	}

	state.groupStack = groupStack[:0]
	return payload, httpReq, errType, errMsg, stackStr, dynamicLabels
}

func (h *jsonHandler) emitJSON(
	r slog.Record,
	jsonPayload map[string]any,
	httpReq *HTTPRequest,
	sourceLoc *sourceLocation,
	fmtTrace, rawTrace, spanID string,
	sampled bool,
	errType, errMsg, stackStr string,
) error {
	if h.writer == nil {
		return errors.New("slogcp: no writer configured")
	}

	jsonPayload["severity"] = levelToString(r.Level)
	jsonPayload[messageKey] = r.Message
	jsonPayload["time"] = r.Time.UTC().Format(time.RFC3339Nano)

	if sourceLoc != nil {
		jsonPayload["logging.googleapis.com/sourceLocation"] = sourceLoc
	}

	if fmtTrace != "" {
		jsonPayload[TraceKey] = fmtTrace
		jsonPayload[SpanKey] = spanID
		jsonPayload[SampledKey] = sampled
	} else if rawTrace != "" {
		jsonPayload["otel.trace_id"] = rawTrace
		jsonPayload["otel.span_id"] = spanID
		jsonPayload["otel.trace_sampled"] = sampled
	}

	if errType != "" {
		jsonPayload["error_type"] = errType
		jsonPayload[messageKey] = fmt.Sprintf("%s: %s", r.Message, errMsg)
	}

	if stackStr != "" {
		jsonPayload[stackTraceKey] = stackStr
	}

	if len(h.cfg.runtimeServiceContext) > 0 {
		if _, exists := jsonPayload["serviceContext"]; !exists {
			if h.cfg.runtimeServiceContextAny != nil {
				jsonPayload["serviceContext"] = h.cfg.runtimeServiceContextAny
			} else {
				jsonPayload["serviceContext"] = stringMapToAny(h.cfg.runtimeServiceContext)
			}
		}
	}

	if httpReq != nil {
		if m := flattenHTTPRequestToMap(httpReq); m != nil {
			jsonPayload[httpRequestKey] = m
		}
	}

	if err := encodeJSON(h.writer, jsonPayload); err != nil {
		h.internalLogger.Error("failed to write JSON log entry", slog.Any("error", err))
		return err
	}
	return nil
}

func captureAndFormatFallbackStack() string {
	stack, _ := CaptureStack(nil)
	return stack
}
