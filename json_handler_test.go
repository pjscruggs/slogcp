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
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

type failingWriter struct {
	err error
}

// Write implements io.Writer for failingWriter and always returns the preset error.
func (f failingWriter) Write(p []byte) (int, error) {
	return 0, f.err
}

// TestResolverHooksFallbackAndCustom exercises the defaulting logic for the hook setters/getters.
func TestResolverHooksFallbackAndCustom(t *testing.T) {
	origSource := getRecordSourceFunc()
	origFrame := getRuntimeFrameResolver()
	t.Cleanup(func() {
		setRecordSourceFunc(origSource)
		setRuntimeFrameResolver(origFrame)
	})

	recordSourceFunc = atomic.Value{}
	pc, _, _, _ := runtime.Caller(0)
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "pc-record", pc)
	srcFn := getRecordSourceFunc()
	if src := srcFn(rec); src == nil || src.Function == "" {
		t.Fatalf("expected fallback source resolver to populate function, got %+v", src)
	}

	custom := func(uintptr) runtime.Frame { return runtime.Frame{Function: "custom-frame"} }
	setRuntimeFrameResolver(custom)
	frame := getRuntimeFrameResolver()(pc)
	if frame.Function != "custom-frame" {
		t.Fatalf("custom frame resolver not used, got %+v", frame)
	}

	runtimeFrameResolver = atomic.Value{}
	frame = getRuntimeFrameResolver()(pc)
	if frame.Function == "" {
		t.Fatalf("expected default frame resolver to populate function")
	}
}

// TestInitialHelperBranches ensures the helper functions exercise both branch paths.
func TestInitialHelperBranches(t *testing.T) {
	cfg := &handlerConfig{
		Writer: io.Discard,
		InitialGroupedAttrs: []groupedAttr{
			{groups: []string{"g"}, attr: slog.String("k", "v")},
		},
		InitialAttrs: []slog.Attr{slog.String("plain", "v")},
	}
	if got := initialGroupedCapacity(cfg); got != 1 {
		t.Fatalf("initialGroupedCapacity grouped branch = %d, want 1", got)
	}
	cfg.InitialGroupedAttrs = nil
	if got := initialGroupedCapacity(cfg); got != 1 {
		t.Fatalf("initialGroupedCapacity fallback branch = %d, want 1", got)
	}

	handler := &jsonHandler{groups: []string{"base"}}
	handler.appendInitialGroups(nil)
	if len(handler.groups) != 1 || handler.groups[0] != "base" {
		t.Fatalf("appendInitialGroups mutated groups on empty input: %#v", handler.groups)
	}

	grouped := handler.cloneGroupedAttrs([]groupedAttr{
		{attr: slog.Attr{}},
		{groups: []string{"keep"}, attr: slog.String("env", "prod")},
	})
	if len(grouped) != 1 || grouped[0].groups[0] != "keep" || grouped[0].attr.Key != "env" {
		t.Fatalf("cloneGroupedAttrs result = %#v", grouped)
	}

	handler.groups = []string{"root"}
	ungrouped := handler.cloneUngroupedAttrs([]slog.Attr{
		{},
		{Key: "", Value: slog.AnyValue(nil)},
		slog.String("child", "v"),
	})
	if len(ungrouped) != 1 || len(ungrouped[0].groups) != 1 || ungrouped[0].groups[0] != "root" {
		t.Fatalf("cloneUngroupedAttrs result = %#v", ungrouped)
	}

	groupedAttrs := handler.collectInitialAttrs(&handlerConfig{
		Writer: io.Discard,
		InitialGroupedAttrs: []groupedAttr{
			{groups: []string{"cg"}, attr: slog.Int("i", 1)},
		},
		InitialAttrs: []slog.Attr{slog.String("ignored", "x")},
	})
	if len(groupedAttrs) != 1 || groupedAttrs[0].attr.Key != "i" {
		t.Fatalf("collectInitialAttrs grouped branch = %#v", groupedAttrs)
	}
	plainAttrs := handler.collectInitialAttrs(&handlerConfig{
		Writer:       io.Discard,
		InitialAttrs: []slog.Attr{slog.String("plain", "v")},
	})
	if len(plainAttrs) != 1 || plainAttrs[0].attr.Key != "plain" {
		t.Fatalf("collectInitialAttrs ungrouped branch = %#v", plainAttrs)
	}
}

// TestEnabledRespectsLeveler covers both the default and configured leveler paths.
func TestEnabledRespectsLeveler(t *testing.T) {
	handler := &jsonHandler{leveler: nil}
	if !handler.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatalf("expected info level to be enabled with default leveler")
	}
	if handler.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatalf("expected debug to be disabled when default level is info")
	}

	levelVar := new(slog.LevelVar)
	levelVar.Set(slog.LevelError)
	handler.leveler = levelVar
	if handler.Enabled(context.Background(), slog.LevelWarn) {
		t.Fatalf("warn should be disabled when leveler is error")
	}
	if !handler.Enabled(context.Background(), slog.LevelError) {
		t.Fatalf("error should be enabled when leveler is error")
	}
}

// TestResolveResolvedValueHandlesUnknownKinds covers the default branch in resolveResolvedValue.
func TestResolveResolvedValueHandlesUnknownKinds(t *testing.T) {
	if val := resolveResolvedValue(slog.GroupValue()); val != nil {
		t.Fatalf("expected nil for group value, got %#v", val)
	}
}

// TestEnsureGroupMapBranches covers existing, replacement, and empty-key paths.
func TestEnsureGroupMapBranches(t *testing.T) {
	state := &payloadState{}
	pb := &payloadBuilder{state: state}

	parent := map[string]any{
		"existing": map[string]any{"kept": 1},
		"string":   "value",
	}

	reused := pb.ensureGroupMap(parent, "existing", 2)
	if reused["kept"] != 1 {
		t.Fatalf("existing map not reused: %#v", reused)
	}

	replaced := pb.ensureGroupMap(parent, "string", 1)
	if _, ok := parent["string"].(map[string]any); !ok {
		t.Fatalf("scalar value not replaced with map: %#v", parent["string"])
	}
	replaced["fresh"] = "value"
	if parent["string"].(map[string]any)["fresh"] != "value" {
		t.Fatalf("ensureGroupMap did not return live map reference: %#v", parent["string"])
	}
	got := pb.ensureGroupMap(parent, "", 1)
	got["added"] = 42
	if parent["added"] != 42 {
		t.Fatalf("empty key should return parent map reference")
	}
}

// TestPayloadBuilderWalkAttrBranches drives the attribute walker through its key branches.
func TestPayloadBuilderWalkAttrBranches(t *testing.T) {
	state := &payloadState{}
	req := &HTTPRequest{RequestMethod: http.MethodGet}
	handler := &jsonHandler{
		cfg: &handlerConfig{
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				if a.Key == httpRequestKey {
					return slog.Any(httpRequestKey, req)
				}
				if len(groups) > 0 && a.Key == "to_replace" {
					return slog.String("replaced", groups[0])
				}
				return a
			},
		},
	}

	pb := &payloadBuilder{
		handler:    handler,
		state:      state,
		record:     slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0),
		payload:    state.prepare(8),
		groupStack: state.groupStack[:0],
	}

	baseMap := pb.payload
	pb.groupStack = append(pb.groupStack, "base")

	pb.walkAttr(1, baseMap, slog.Attr{}, false)

	pb.walkAttr(1, baseMap, slog.Attr{Key: httpRequestKey, Value: slog.StringValue("primary")}, false)
	pb.walkAttr(1, baseMap, slog.Attr{Key: httpRequestKey, Value: slog.StringValue("secondary")}, false)

	pb.walkAttr(1, baseMap, slog.Attr{Key: LabelsGroup, Value: slog.GroupValue(slog.String("region", "us-central1"))}, false)
	pb.walkAttr(1, baseMap, slog.Attr{Key: "grouped", Value: slog.GroupValue(slog.String("nested", "v"))}, false)

	pb.walkAttr(1, baseMap, slog.Any("error_attr", errors.New("boom")), false)
	pb.walkAttr(1, baseMap, slog.Any("to_replace", errors.New("boom")), false)

	if pb.httpReq == nil || pb.httpReq.RequestMethod != http.MethodGet {
		t.Fatalf("http request not captured: %#v", pb.httpReq)
	}
	if pb.firstErr == nil || pb.firstErr.Error() != "boom" {
		t.Fatalf("firstErr not captured, got %#v", pb.firstErr)
	}

	labels := pb.ensureLabels()
	if labels["region"] != "us-central1" {
		t.Fatalf("labels not populated: %#v", labels)
	}
	grouped, ok := baseMap["grouped"].(map[string]any)
	if !ok || grouped["nested"] != "v" {
		t.Fatalf("grouped map missing nested value: %#v", baseMap["grouped"])
	}
	if got := baseMap["replaced"]; got != "base" {
		t.Fatalf("ReplaceAttr branch not applied, got %#v", got)
	}
	if got := baseMap[httpRequestKey]; got != req {
		t.Fatalf("secondary httpRequest attr should retain request, got %#v", got)
	}
}

// TestPayloadBuilderWalkAttrStoresSecondaryHTTPRequestValue ensures non-request httpRequest values persist.
func TestPayloadBuilderWalkAttrStoresSecondaryHTTPRequestValue(t *testing.T) {
	state := &payloadState{}
	handler := &jsonHandler{cfg: &handlerConfig{}}

	pb := &payloadBuilder{
		handler:    handler,
		state:      state,
		record:     slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0),
		payload:    state.prepare(4),
		groupStack: state.groupStack[:0],
	}

	req := &HTTPRequest{RequestMethod: http.MethodGet}
	pb.walkAttr(0, pb.payload, slog.Attr{Key: httpRequestKey, Value: slog.AnyValue(req)}, false)
	pb.walkAttr(0, pb.payload, slog.Attr{Key: httpRequestKey, Value: slog.StringValue("secondary")}, false)

	if pb.httpReq == nil || pb.httpReq.RequestMethod != http.MethodGet {
		t.Fatalf("http request not captured: %#v", pb.httpReq)
	}
	if got := pb.payload[httpRequestKey]; got != "secondary" {
		t.Fatalf("secondary httpRequest attr should be stored as value, got %#v", got)
	}
}

// TestResolveErrorAndStackVariants covers stack capture with and without errors.
func TestResolveErrorAndStackVariants(t *testing.T) {
	state := &payloadState{}
	handler := &jsonHandler{
		cfg: &handlerConfig{
			StackTraceEnabled: true,
			StackTraceLevel:   slog.LevelInfo,
		},
	}

	record := slog.NewRecord(time.Now(), slog.LevelError, "err", 0)
	pb := &payloadBuilder{
		handler: handler,
		state:   state,
		record:  record,
	}
	pb.firstErr = errors.New("boom")
	errType, errMsg := pb.resolveErrorAndStack()
	if errType == "" || errMsg == "" {
		t.Fatalf("missing error metadata: type=%q msg=%q", errType, errMsg)
	}
	if pb.stackStr == "" {
		t.Fatalf("stackStr not captured for error path")
	}

	pb.firstErr = nil
	pb.stackStr = ""
	pb.record = slog.NewRecord(time.Now(), slog.LevelInfo, "stack-only", 0)
	errType, errMsg = pb.resolveErrorAndStack()
	if errType != "" || errMsg != "" {
		t.Fatalf("unexpected error metadata without firstErr: type=%q msg=%q", errType, errMsg)
	}
	if pb.stackStr == "" {
		t.Fatalf("stackStr not captured when stack tracing enabled without error")
	}
}

// TestApplyTraceBranches ensures applyTrace routes each switch arm.
func TestApplyTraceBranches(t *testing.T) {
	formatted := &jsonHandler{cfg: &handlerConfig{}}
	payload := map[string]any{}
	formatted.applyTrace(payload, "projects/p/traces/t", "raw", "01", true, true)
	if payload[TraceKey] != "projects/p/traces/t" || payload[SpanKey] != "01" || payload[SampledKey] != true {
		t.Fatalf("formatted trace path produced %#v", payload)
	}

	auto := &jsonHandler{cfg: &handlerConfig{traceAllowAutoformat: true}}
	payload = map[string]any{}
	auto.applyTrace(payload, "", "raw-trace", "02", false, false)
	if payload[TraceKey] != "raw-trace" {
		t.Fatalf("autoformat trace path produced %#v", payload)
	}
	if _, exists := payload[SpanKey]; exists {
		t.Fatalf("spanId should be omitted when span not owned: %#v", payload)
	}
	if payload[SampledKey] != false {
		t.Fatalf("sampled flag mismatch in autoformat path: %#v", payload)
	}

	unknown := &jsonHandler{cfg: &handlerConfig{}}
	payload = map[string]any{}
	unknown.applyTrace(payload, "", "fallback", "03", false, true)
	if payload["otel.trace_id"] != "fallback" || payload["otel.span_id"] != "03" || payload["otel.trace_sampled"] != true {
		t.Fatalf("unknown project path produced %#v", payload)
	}
	if _, exists := payload[TraceKey]; exists {
		t.Fatalf("TraceKey should not be set for unknown project path: %#v", payload)
	}

	none := &jsonHandler{cfg: &handlerConfig{}}
	payload = map[string]any{"existing": 1}
	none.applyTrace(payload, "", "", "", false, false)
	if len(payload) != 1 || payload["existing"] != 1 {
		t.Fatalf("applyTrace should leave payload untouched when trace IDs empty, got %#v", payload)
	}
}

// TestApplySeverityServiceContextAndHTTPRequest covers remaining helper branches.
func TestApplySeverityServiceContextAndHTTPRequest(t *testing.T) {
	record := slog.NewRecord(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), slog.LevelWarn, "msg", 0)

	handler := &jsonHandler{cfg: &handlerConfig{UseShortSeverityNames: true}}
	payload := map[string]any{}
	handler.applySeverityAndTime(payload, record)
	if payload["severity"] == "" {
		t.Fatalf("severity not populated")
	}
	if _, exists := payload["time"]; exists {
		t.Fatalf("time should be omitted when EmitTimeField is false")
	}

	handler.cfg.EmitTimeField = true
	payload = map[string]any{}
	handler.applySeverityAndTime(payload, record)
	if _, exists := payload["time"]; !exists {
		t.Fatalf("time should be emitted when EmitTimeField is true")
	}

	handler.cfg.runtimeServiceContext = map[string]string{"service": "checkout"}
	handler.cfg.runtimeServiceContextAny = map[string]any{"service": "any", "version": "v2"}

	payload = map[string]any{}
	handler.applyServiceContext(payload)
	if svc := payload["serviceContext"].(map[string]any)["service"]; svc != "any" {
		t.Fatalf("runtimeServiceContextAny should take precedence, got %v", svc)
	}

	payload = map[string]any{"serviceContext": map[string]any{"service": "existing"}}
	handler.applyServiceContext(payload)
	if svc := payload["serviceContext"].(map[string]any)["service"]; svc != "existing" {
		t.Fatalf("existing serviceContext should not be overwritten, got %v", svc)
	}

	handler.cfg.runtimeServiceContextAny = nil
	handler.cfg.runtimeServiceContext = nil
	payload = map[string]any{}
	handler.applyServiceContext(payload)
	if len(payload) != 0 {
		t.Fatalf("serviceContext should be omitted when runtime context is empty, got %#v", payload)
	}

	handler.applyHTTPRequest(payload, nil)
	if len(payload) != 0 {
		t.Fatalf("applyHTTPRequest should skip nil requests, got %#v", payload)
	}

	req := &HTTPRequest{RequestMethod: http.MethodPost}
	handler.applyHTTPRequest(payload, req)
	if _, exists := payload[httpRequestKey]; !exists {
		t.Fatalf("httpRequest key missing after applyHTTPRequest: %#v", payload)
	}
}

// TestWriteJSONPayloadBranches covers pooled, unpooled, and error paths.
func TestWriteJSONPayloadBranches(t *testing.T) {
	baseCfg := &handlerConfig{Writer: io.Discard}
	logger := slog.New(slog.DiscardHandler)

	handler := &jsonHandler{
		mu:             &sync.Mutex{},
		cfg:            baseCfg,
		writer:         io.Discard,
		internalLogger: logger,
		bufferPool:     &jsonBufferPool,
	}

	if err := handler.writeJSONPayload(map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatalf("expected encode error for unsupported value")
	}

	handler.writer = failingWriter{err: errors.New("write failed")}
	if err := handler.writeJSONPayload(map[string]any{"ok": "value"}); err == nil {
		t.Fatalf("expected write error to be returned")
	}

	var buf bytes.Buffer
	handler.writer = &buf
	handler.bufferPool = nil
	if err := handler.writeJSONPayload(map[string]any{"ok": "value"}); err != nil {
		t.Fatalf("writeJSONPayload returned %v", err)
	}
	if !strings.Contains(buf.String(), `"ok"`) {
		t.Fatalf("buffer missing encoded payload: %q", buf.String())
	}
}

// TestStackTraceComparisonLevelDefault ensures LevelDefault normalizes to info for comparisons.
func TestStackTraceComparisonLevelDefault(t *testing.T) {
	if got := stackTraceComparisonLevel(slog.Level(LevelDefault)); got != slog.LevelInfo {
		t.Fatalf("stackTraceComparisonLevel(LevelDefault) = %v, want %v", got, slog.LevelInfo)
	}
	if got := stackTraceComparisonLevel(slog.LevelError); got != slog.LevelError {
		t.Fatalf("stackTraceComparisonLevel(LevelError) = %v, want LevelError", got)
	}
}

// TestPruneEmptyMapsFullyPrunes covers the branch where the root map becomes empty.
func TestPruneEmptyMapsFullyPrunes(t *testing.T) {
	payload := map[string]any{
		"empty": map[string]any{},
	}
	if !pruneEmptyMaps(payload) {
		t.Fatalf("expected pruneEmptyMaps to report map as empty")
	}
	if len(payload) != 0 {
		t.Fatalf("payload should be empty after pruning, got %#v", payload)
	}
}

// TestResolverHooksCoverCustomAndDefault verifies both cached and fallback resolver branches.
func TestResolverHooksCoverCustomAndDefault(t *testing.T) {
	origSource := getRecordSourceFunc()
	origFrame := getRuntimeFrameResolver()
	t.Cleanup(func() {
		setRecordSourceFunc(origSource)
		setRuntimeFrameResolver(origFrame)
	})

	recordSourceFunc = atomic.Value{}
	if src := getRecordSourceFunc()(slog.Record{}); src != nil {
		t.Fatalf("expected default record source to return nil, got %+v", src)
	}

	pc, _, _, _ := runtime.Caller(0)
	defFrame := getRuntimeFrameResolver()(pc)
	if defFrame.Function == "" {
		t.Fatalf("default frame resolver did not populate function")
	}

	setRecordSourceFunc(func(slog.Record) *slog.Source {
		return &slog.Source{Function: "custom-source"}
	})
	if src := getRecordSourceFunc()(slog.NewRecord(time.Now(), slog.LevelInfo, "msg", pc)); src.Function != "custom-source" {
		t.Fatalf("custom record source not used, got %+v", src)
	}

	setRuntimeFrameResolver(func(uintptr) runtime.Frame {
		return runtime.Frame{File: "hooked.go", Line: 7, Function: "hooked"}
	})
	customFrame := getRuntimeFrameResolver()(pc)
	if customFrame.Function != "hooked" || customFrame.Line != 7 || customFrame.File != "hooked.go" {
		t.Fatalf("custom frame resolver not used, got %+v", customFrame)
	}
}

// TestJSONHandlerHandleBuildsComplexPayload exercises the full payload pipeline, including labels,
// trace/error handling, grouping, and service context application.
func TestJSONHandlerHandleBuildsComplexPayload(t *testing.T) {
	var buf bytes.Buffer

	cfg := &handlerConfig{
		Writer:            &buf,
		AddSource:         true,
		EmitTimeField:     true,
		StackTraceEnabled: true,
		StackTraceLevel:   slog.LevelInfo,
		runtimeServiceContext: map[string]string{
			"service": "svc",
			"version": "v1",
		},
		traceAllowAutoformat:  true,
		traceDiagnosticsState: newTraceDiagnostics(TraceDiagnosticsWarnOnce),
		InitialGroupedAttrs: []groupedAttr{
			{attr: slog.String("init", "yes")},
			{groups: []string{"grouped"}, attr: slog.String("inside", "value")},
			{groups: []string{LabelsGroup}, attr: slog.String("base_label", "yes")},
		},
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if attr.Key == "swap" {
				joined := strings.Join(groups, "/")
				if joined == "" {
					joined = "rootless"
				}
				return slog.String("replaced", joined)
			}
			if attr.Key == "drop-empty" {
				return slog.Attr{}
			}
			return attr
		},
	}

	handler := newJSONHandler(cfg, slog.LevelWarn, slog.New(slog.DiscardHandler))
	child := handler.WithGroup("child").(*jsonHandler)
	if child == handler {
		t.Fatalf("WithGroup should return a new handler instance")
	}
	withAttrs := handler.WithAttrs([]slog.Attr{slog.String("extra", "attr")}).(*jsonHandler)
	if withAttrs == handler {
		t.Fatalf("WithAttrs should return a new handler instance")
	}

	if handler.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatalf("info should be disabled when handler level is warn")
	}
	if !handler.Enabled(context.Background(), slog.LevelWarn) {
		t.Fatalf("warn should be enabled when handler level is warn")
	}

	pc, _, _, _ := runtime.Caller(0)
	infoRecord := slog.NewRecord(time.Now(), slog.LevelInfo, "ignored", pc)
	if err := handler.Handle(context.Background(), infoRecord); err != nil {
		t.Fatalf("Handle(info) returned %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no output for disabled level, got %q", buf.String())
	}

	record := slog.NewRecord(time.Now(), slog.LevelWarn, "coverage", pc)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, "http://example.com", nil)
	httpAttr := slog.Attr{Key: httpRequestKey, Value: slog.AnyValue(HTTPRequestFromRequest(req))}
	record.AddAttrs(
		slog.String("swap", "value"),
		slog.String("drop-empty", "ignored"),
		slog.Group("nested", slog.String("child", "v")),
		slog.Group(LabelsGroup, slog.String("dynamic", "label"), slog.Any("nil-value", nil)),
		httpAttr,
		slog.Any("err", errors.New("kaboom")),
		slog.Attr{Key: "", Value: slog.StringValue("skip")},
	)

	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle(warn) returned %v", err)
	}

	entry := decodeJSONLine(t, buf.String())

	labels, ok := entry[labelsGroupKey].(map[string]any)
	if !ok {
		t.Fatalf("labels missing from payload: %#v", entry)
	}
	for _, key := range []string{"dynamic", "base_label"} {
		if _, exists := labels[key]; !exists {
			t.Fatalf("label %q missing from payload: %#v", key, labels)
		}
	}
	if _, exists := labels["nil-value"]; exists {
		t.Fatalf("nil label value should be skipped: %#v", labels)
	}

	grouped, ok := entry["grouped"].(map[string]any)
	if !ok || grouped["inside"] != "value" {
		t.Fatalf("grouped attr missing: %#v", entry["grouped"])
	}

	if entry["init"] != "yes" {
		t.Fatalf("initial attr not propagated, got %#v (entry=%#v)", entry["init"], entry)
	}
	if entry["replaced"] != "rootless" {
		t.Fatalf("ReplaceAttr not applied, got %#v", entry["replaced"])
	}

	nested, ok := entry["nested"].(map[string]any)
	if !ok || nested["child"] != "v" {
		t.Fatalf("nested group missing data: %#v", entry["nested"])
	}
	if entry["err"] != "kaboom" {
		t.Fatalf("error value not resolved, got %#v", entry["err"])
	}

	if msg, ok := entry[messageKey].(string); !ok || !strings.Contains(msg, "coverage: kaboom") {
		t.Fatalf("message not adjusted for error: %#v", entry[messageKey])
	}

	if _, ok := entry[stackTraceKey]; !ok {
		t.Fatalf("stack_trace missing from payload: %#v", entry)
	}

	httpPayload, ok := entry[httpRequestKey].(map[string]any)
	if !ok || httpPayload["requestMethod"] != http.MethodPut {
		t.Fatalf("httpRequest not flattened: %#v", entry[httpRequestKey])
	}

	if svc, ok := entry["serviceContext"].(map[string]any); !ok || svc["service"] != "svc" {
		t.Fatalf("serviceContext missing or incorrect: %#v", entry["serviceContext"])
	}

	if loc, ok := entry["logging.googleapis.com/sourceLocation"].(map[string]any); !ok || loc["function"] == "" {
		t.Fatalf("sourceLocation missing or incomplete: %#v", entry["logging.googleapis.com/sourceLocation"])
	}
}

// TestJSONHandlerBranchSweep walks helper branches to cover remaining edge cases.
func TestJSONHandlerBranchSweep(t *testing.T) {
	origSource := getRecordSourceFunc()
	origFrame := getRuntimeFrameResolver()
	t.Cleanup(func() {
		setRecordSourceFunc(origSource)
		setRuntimeFrameResolver(origFrame)
	})

	runtimeFrameResolver = atomic.Value{}
	frameResolver := getRuntimeFrameResolver()
	pc, _, _, _ := runtime.Caller(0)
	if frame := frameResolver(pc); frame.Function == "" && frame.File == "" {
		t.Fatalf("fallback runtime frame resolver returned empty frame: %+v", frame)
	}
	setRuntimeFrameResolver(nil)
	setRuntimeFrameResolver(func(uintptr) runtime.Frame { return runtime.Frame{Function: "custom"} })
	if frame := getRuntimeFrameResolver()(pc); frame.Function != "custom" {
		t.Fatalf("custom runtime frame resolver not used, got %+v", frame)
	}

	recordSourceFunc = atomic.Value{}
	_ = getRecordSourceFunc()(slog.Record{})
	setRecordSourceFunc(func(slog.Record) *slog.Source { return &slog.Source{Function: "src"} })
	if src := getRecordSourceFunc()(slog.NewRecord(time.Now(), slog.LevelInfo, "msg", pc)); src.Function != "src" {
		t.Fatalf("custom record source resolver not used, got %+v", src)
	}

	var buf bytes.Buffer
	cfg := &handlerConfig{
		Writer:            &buf,
		AddSource:         true,
		EmitTimeField:     true,
		StackTraceEnabled: true,
		StackTraceLevel:   slog.LevelDebug,
		runtimeServiceContext: map[string]string{
			"service": "svc",
			"version": "v1",
		},
		runtimeServiceContextAny: map[string]any{"service": "override"},
		traceAllowAutoformat:     true,
		traceDiagnosticsState:    newTraceDiagnostics(TraceDiagnosticsWarnOnce),
		InitialGroups:            []string{"base"},
		InitialAttrs:             []slog.Attr{slog.String("init", "yes")},
		InitialGroupedAttrs: []groupedAttr{
			{groups: []string{"grouped"}, attr: slog.String("inside", "value")},
			{groups: []string{LabelsGroup}, attr: slog.String("label", "from-base")},
		},
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if attr.Key == "replace" {
				return slog.String("replaced", strings.Join(groups, "+"))
			}
			if attr.Key == "drop" {
				return slog.Attr{}
			}
			return attr
		},
	}

	handler := newJSONHandler(cfg, nil, slog.New(slog.DiscardHandler))
	if handler.leveler.Level() != slog.LevelInfo {
		t.Fatalf("expected default leveler when nil provided")
	}
	if !handler.Enabled(context.Background(), slog.LevelInfo) || handler.Enabled(context.Background(), slog.LevelDebug-1) {
		t.Fatalf("Enabled does not respect default leveler")
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.com", nil)
	record := slog.NewRecord(time.Now(), slog.LevelError, "branch", pc)
	record.AddAttrs(
		slog.Attr{},
		slog.Any("replace", "value"),
		slog.Any("drop", "ignored"),
		slog.Group("grp", slog.String("child", "v")),
		slog.Group("", slog.String("ignored", "v")),
		slog.Group(LabelsGroup, slog.Int("answer", 42), slog.Any("nil", nil)),
		slog.Any(httpRequestKey, HTTPRequestFromRequest(req)),
		slog.Any(httpRequestKey, "raw"),
		slog.Any("err", errors.New("boom")),
	)

	state := &payloadState{}
	payload, httpReq, errType, errMsg, stackStr, dynLabels := handler.buildPayload(record, state)
	if errType == "" || errMsg == "" || stackStr == "" {
		t.Fatalf("expected error metadata, got errType=%q errMsg=%q stackStr=%q", errType, errMsg, stackStr)
	}
	if dynLabels["label"] != "from-base" || dynLabels["answer"] != "42" {
		t.Fatalf("expected labels from base and record, got %#v", dynLabels)
	}
	if _, ok := dynLabels["nil"]; ok {
		t.Fatalf("nil label value should be skipped: %#v", dynLabels)
	}

	parent := map[string]any{"collide": "value"}
	builder := &payloadBuilder{state: state}
	ensure := builder.ensureGroupMap(parent, "collide", 1)
	ensure["new"] = "ok"
	if _, ok := parent["collide"].(map[string]any)["new"]; !ok {
		t.Fatalf("ensureGroupMap did not replace non-map value: %#v", parent["collide"])
	}

	handler.cfg.StackTraceEnabled = false
	builder.handler = handler
	builder.record = slog.NewRecord(time.Now(), slog.LevelInfo, "no-stack", 0)
	if tpe, msg := builder.resolveErrorAndStack(); tpe != "" || msg != "" {
		t.Fatalf("expected empty error metadata when firstErr nil and stack disabled, got %q %q", tpe, msg)
	}
	handler.cfg.StackTraceEnabled = true

	rawReqVal := slog.AnyValue(HTTPRequestFromRequest(req))
	if !builder.handleHTTPRequestAttr(rawReqVal) {
		t.Fatalf("expected handleHTTPRequestAttr to capture request")
	}
	if builder.handleHTTPRequestAttr(rawReqVal) {
		t.Fatalf("expected subsequent handleHTTPRequestAttr call to be ignored when httpReq already set")
	}

	if httpReq == nil {
		httpReq = builder.httpReq
	}
	payload["serviceContext"] = map[string]any{"service": "existing"}
	handler.applyServiceContext(payload)
	if svc := payload["serviceContext"].(map[string]any)["service"]; svc != "existing" {
		t.Fatalf("existing serviceContext should be preserved, got %v", svc)
	}
	delete(payload, "serviceContext")
	handler.applyServiceContext(payload)
	if svc := payload["serviceContext"].(map[string]any)["service"]; svc != "override" {
		t.Fatalf("runtimeServiceContextAny should win, got %v", svc)
	}

	sourceLoc := handler.resolveSourceLocation(record)
	buf.Reset()
	if err := handler.emitJSON(record, payload, httpReq, sourceLoc, "projects/demo/traces/t123", "raw", "span1", true, false, errType, errMsg, stackStr); err != nil {
		t.Fatalf("emitJSON returned %v", err)
	}
	entry := decodeJSONLine(t, buf.String())
	if entry[TraceKey] != "projects/demo/traces/t123" || entry[SpanKey] != "span1" {
		t.Fatalf("trace fields missing from emitJSON: %#v", entry)
	}

	handler.cfg.traceAllowAutoformat = false
	payload2 := map[string]any{}
	handler.applyTrace(payload2, "", "raw-only", "span2", false, true)
	if payload2["otel.trace_id"] != "raw-only" || payload2["otel.trace_sampled"] != true {
		t.Fatalf("applyTrace unknown project branch not applied: %#v", payload2)
	}

	payload3 := map[string]any{}
	if msg := handler.applyErrorFields(payload3, "plain", "", "", ""); msg != "plain" || len(payload3) != 0 {
		t.Fatalf("applyErrorFields should leave message untouched when no error, got %q payload=%#v", msg, payload3)
	}
}

// TestJSONHandlerHelperBranchesExplicit covers small helper branches directly.
func TestJSONHandlerHelperBranchesExplicit(t *testing.T) {
	t.Run("initialHelpers", func(t *testing.T) {
		cfg := &handlerConfig{
			Writer: io.Discard,
			InitialGroupedAttrs: []groupedAttr{
				{groups: []string{"g"}, attr: slog.String("k", "v")},
			},
			InitialAttrs: []slog.Attr{slog.String("plain", "v")},
		}
		if initialGroupedCapacity(cfg) == 0 {
			t.Fatalf("initialGroupedCapacity returned zero for grouped attrs")
		}
		h := &jsonHandler{groups: []string{"grp"}}
		grouped := h.collectInitialAttrs(cfg)
		if len(grouped) != 1 || grouped[0].attr.Key != "k" {
			t.Fatalf("collectInitialAttrs grouped = %#v", grouped)
		}
		cfg.InitialGroupedAttrs = nil
		ungrouped := h.collectInitialAttrs(cfg)
		if len(ungrouped) != 1 || ungrouped[0].attr.Key != "plain" || ungrouped[0].groups[0] != "grp" {
			t.Fatalf("collectInitialAttrs ungrouped = %#v", ungrouped)
		}
	})

	t.Run("applyHelpers", func(t *testing.T) {
		var buf bytes.Buffer
		handler := &jsonHandler{
			mu: &sync.Mutex{},
			cfg: &handlerConfig{
				EmitTimeField:            true,
				UseShortSeverityNames:    true,
				runtimeServiceContext:    map[string]string{"service": "svc"},
				runtimeServiceContextAny: map[string]any{"service": "svc-any"},
				traceAllowAutoformat:     true,
				traceDiagnosticsState:    newTraceDiagnostics(TraceDiagnosticsWarnOnce),
			},
			writer:         &buf,
			internalLogger: slog.New(slog.DiscardHandler),
			bufferPool:     &jsonBufferPool,
		}

		record := slog.NewRecord(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), slog.LevelWarn, "helper", 0)
		payload := map[string]any{}
		handler.applySeverityAndTime(payload, record)
		handler.applySourceLocation(payload, &sourceLocation{File: "f.go", Line: 1, Function: "fn"})
		handler.applyTrace(payload, "projects/demo/traces/trace", "raw", "span", true, true)
		handler.applyTrace(payload, "", "raw-trace", "span2", false, false)
		handler.cfg.traceAllowAutoformat = false
		handler.applyTrace(payload, "", "raw-unknown", "span3", false, true)
		msg := handler.applyErrorFields(payload, "msg", "Type", "err", "stack")
		if msg != "msg: err" {
			t.Fatalf("applyErrorFields adjusted message = %q", msg)
		}
		handler.applyServiceContext(payload)
		handler.applyHTTPRequest(payload, &HTTPRequest{RequestMethod: http.MethodGet})

		if err := handler.writeJSONPayload(payload); err != nil {
			t.Fatalf("writeJSONPayload returned %v", err)
		}
		if buf.Len() == 0 {
			t.Fatalf("writeJSONPayload did not write output")
		}
	})
}

// TestEmitJSONHandlesWriterAndTrace verifies emitJSON error handling and trace/service helpers.
func TestEmitJSONHandlesWriterAndTrace(t *testing.T) {
	handler := &jsonHandler{
		mu: &sync.Mutex{},
		cfg: &handlerConfig{
			EmitTimeField:         true,
			UseShortSeverityNames: true,
			runtimeServiceContext: map[string]string{
				"service": "svc",
			},
			traceAllowAutoformat: true,
		},
		internalLogger: slog.New(slog.DiscardHandler),
		bufferPool:     &jsonBufferPool,
	}

	err := handler.emitJSON(
		slog.NewRecord(time.Now(), slog.LevelError, "no-writer", 0),
		map[string]any{},
		nil,
		nil,
		"",
		"",
		"",
		false,
		false,
		"",
		"",
		"",
	)
	if err == nil {
		t.Fatalf("expected emitJSON to fail when writer is nil")
	}

	var buf bytes.Buffer
	handler.writer = &buf
	record := slog.NewRecord(time.Now(), slog.LevelError, "emit", 0)
	sourceLoc := &sourceLocation{File: "f.go", Line: 12, Function: "fn"}
	httpReq := &HTTPRequest{RequestMethod: http.MethodGet}

	if err := handler.emitJSON(
		record,
		map[string]any{"custom": 1},
		httpReq,
		sourceLoc,
		"projects/demo/traces/t1",
		"raw-trace",
		"abcd1234",
		true,
		true,
		"Type",
		"msg",
		"stack",
	); err != nil {
		t.Fatalf("emitJSON returned %v", err)
	}

	entry := decodeJSONLine(t, buf.String())
	if entry[TraceKey] != "projects/demo/traces/t1" || entry[SpanKey] != "abcd1234" || entry[SampledKey] != true {
		t.Fatalf("trace fields not applied: %#v", entry)
	}
	if entry[stackTraceKey] != "stack" {
		t.Fatalf("stackTrace not applied: %#v", entry[stackTraceKey])
	}
	if msg := entry[messageKey]; msg != "emit: msg" {
		t.Fatalf("message not adjusted, got %#v", msg)
	}
	if _, ok := entry["serviceContext"]; !ok {
		t.Fatalf("serviceContext missing from payload: %#v", entry)
	}
	if _, ok := entry[httpRequestKey]; !ok {
		t.Fatalf("httpRequest missing from payload: %#v", entry)
	}
	if loc, ok := entry["logging.googleapis.com/sourceLocation"].(map[string]any); !ok || loc["file"] != "f.go" {
		t.Fatalf("sourceLocation not applied: %#v", entry["logging.googleapis.com/sourceLocation"])
	}
}

// TestJSONHandlerBranchesExhaustive walks through helper branches that were previously
// unexercised to push coverage to 100%.
func TestJSONHandlerBranchesExhaustive(t *testing.T) {
	origSource := getRecordSourceFunc()
	origFrame := getRuntimeFrameResolver()
	t.Cleanup(func() {
		setRecordSourceFunc(origSource)
		setRuntimeFrameResolver(origFrame)
	})

	setRecordSourceFunc(nil)
	_ = getRecordSourceFunc()(slog.Record{})
	setRecordSourceFunc(func(r slog.Record) *slog.Source { return r.Source() })

	setRuntimeFrameResolver(nil)
	_ = getRuntimeFrameResolver()(0)
	setRuntimeFrameResolver(func(uintptr) runtime.Frame {
		return runtime.Frame{File: "alt.go", Line: 7, Function: "alt"}
	})
	_ = getRuntimeFrameResolver()(0)

	cfg := &handlerConfig{
		AddSource:         true,
		EmitTimeField:     true,
		StackTraceEnabled: true,
		StackTraceLevel:   slog.LevelInfo,
		Writer:            io.Discard,
		InitialGroups:     []string{"g1"},
		InitialAttrs: []slog.Attr{
			{},
			{Key: "plain", Value: slog.StringValue("v")},
		},
		InitialGroupedAttrs: []groupedAttr{
			{attr: slog.Attr{}},
			{groups: []string{"g2"}, attr: slog.String("inner", "v")},
		},
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if attr.Key == "dropper" {
				return slog.Attr{}
			}
			if attr.Key == "swap" {
				return slog.String("replaced", strings.Join(groups, "/"))
			}
			return attr
		},
	}

	base := newJSONHandler(cfg, nil, slog.New(slog.DiscardHandler))
	_ = initialGroupedCapacity(&handlerConfig{InitialAttrs: []slog.Attr{slog.String("x", "y")}})

	_ = base.cloneGroupedAttrs([]groupedAttr{{attr: slog.Attr{}}, {groups: []string{"cg"}, attr: slog.String("k", "v")}})
	base.groups = []string{"base"}
	_ = base.cloneUngroupedAttrs([]slog.Attr{{}, slog.String("ok", "v")})

	var empty jsonHandler
	if !empty.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatalf("Enabled(nil leveler) should default to allow info")
	}

	if got := base.WithAttrs(nil); got != base {
		t.Fatalf("WithAttrs(nil) should return receiver")
	}
	attrHandler, ok := base.WithAttrs([]slog.Attr{slog.String("wa", "wv")}).(*jsonHandler)
	if !ok {
		t.Fatalf("WithAttrs should return *jsonHandler")
	}
	if attrHandler == base || len(attrHandler.groupedAttrs) == len(base.groupedAttrs) {
		t.Fatalf("WithAttrs should expand grouped attrs, got %d", len(attrHandler.groupedAttrs))
	}
	if got := base.WithGroup(""); got != base {
		t.Fatalf("WithGroup(\"\") should return receiver")
	}
	groupHandler, ok := base.WithGroup("child").(*jsonHandler)
	if !ok {
		t.Fatalf("WithGroup should return *jsonHandler")
	}

	var buf bytes.Buffer
	groupHandler.writer = &buf
	groupHandler.cfg.Writer = &buf
	groupHandler.cfg.runtimeServiceContext = map[string]string{"service": "svc", "version": "v1"}

	pc, _, _, _ := runtime.Caller(0)
	record := slog.NewRecord(time.Now(), slog.LevelWarn, "pipeline", pc)
	record.AddAttrs(
		slog.String("swap", "x"),
		slog.String("dropper", "y"),
		slog.Group("grp", slog.String("child", "v")),
		slog.Group(LabelsGroup, slog.String("dynamic", "d"), slog.Any("nilLabel", nil)),
		slog.Any(httpRequestKey, &HTTPRequest{RequestMethod: http.MethodPost}),
		slog.Any("errAttr", errors.New("boom")),
		slog.Attr{Key: "", Value: slog.StringValue("skip")},
	)

	if err := groupHandler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle returned %v", err)
	}
	if buf.Len() == 0 {
		t.Fatalf("expected output from Handle pipeline")
	}

	entry := decodeJSONLine(t, buf.String())
	if entry[messageKey] == "" {
		t.Fatalf("message not emitted: %#v", entry)
	}
	if labels, _ := entry[labelsGroupKey].(map[string]any); len(labels) == 0 {
		t.Fatalf("labels missing from payload: %#v", entry)
	}
}

// TestJSONHandlerEmitAndWriteJSONBranches covers emitJSON and writeJSONPayload success/error paths.
func TestJSONHandlerEmitAndWriteJSONBranches(t *testing.T) {
	t.Run("emitVariants", func(t *testing.T) {
		var buf bytes.Buffer
		cfg := &handlerConfig{
			EmitTimeField:         false,
			UseShortSeverityNames: true,
			traceAllowAutoformat:  true,
			traceDiagnosticsState: newTraceDiagnostics(TraceDiagnosticsWarnOnce),
			runtimeServiceContextAny: map[string]any{
				"service": "svc-any",
			},
			Writer: &buf,
		}
		h := &jsonHandler{
			mu:             &sync.Mutex{},
			cfg:            cfg,
			leveler:        slog.LevelInfo,
			writer:         &buf,
			internalLogger: slog.New(slog.DiscardHandler),
			bufferPool:     &jsonBufferPool,
		}

		src := &sourceLocation{File: "f.go", Line: 1, Function: "fn"}
		if err := h.emitJSON(
			slog.NewRecord(time.Now(), slog.LevelInfo, "emit", 0),
			map[string]any{},
			&HTTPRequest{RequestMethod: http.MethodGet},
			src,
			"projects/demo/traces/t99",
			"raw",
			"span1",
			true,
			true,
			"Type",
			"msg",
			"",
		); err != nil {
			t.Fatalf("emitJSON (cloud trace) = %v", err)
		}

		cfg.traceAllowAutoformat = false
		payload := map[string]any{"serviceContext": map[string]any{"service": "keep"}}
		if err := h.emitJSON(
			slog.NewRecord(time.Now(), slog.LevelError, "emit2", 0),
			payload,
			nil,
			nil,
			"",
			"raw-trace",
			"span2",
			false,
			false,
			"",
			"",
			"",
		); err != nil {
			t.Fatalf("emitJSON (unknown trace) = %v", err)
		}
	})

	t.Run("writeJSONPayloadBranches", func(t *testing.T) {
		okHandler := &jsonHandler{
			mu:             &sync.Mutex{},
			cfg:            &handlerConfig{},
			writer:         &bytes.Buffer{},
			internalLogger: slog.New(slog.DiscardHandler),
			bufferPool:     nil, // exercise non-pooled path
		}
		if err := okHandler.writeJSONPayload(map[string]any{"ok": 1}); err != nil {
			t.Fatalf("writeJSONPayload success path = %v", err)
		}

		errHandler := &jsonHandler{
			mu:             &sync.Mutex{},
			cfg:            &handlerConfig{},
			writer:         io.Discard,
			internalLogger: slog.New(slog.DiscardHandler),
			bufferPool:     &jsonBufferPool,
		}
		if err := errHandler.writeJSONPayload(map[string]any{"bad": func() {}}); err == nil {
			t.Fatalf("expected encoding error from writeJSONPayload")
		}
	})
}

// TestErrorReportingPartialFrames ensures report location helpers emit attrs when partial frames are provided.
func TestErrorReportingPartialFrames(t *testing.T) {
	// Only file populated.
	frame := runtime.Frame{File: "file.go"}
	if attrs := buildReportLocationAttrs(frame); len(attrs) == 0 {
		t.Fatalf("file-only frame should produce reportLocation")
	}
	// Only line populated.
	frame = runtime.Frame{Line: 42}
	if attrs := buildReportLocationAttrs(frame); len(attrs) == 0 {
		t.Fatalf("line-only frame should produce reportLocation")
	}
	// Only function populated.
	frame = runtime.Frame{Function: "fn"}
	if attrs := buildReportLocationAttrs(frame); len(attrs) == 0 {
		t.Fatalf("function-only frame should produce reportLocation")
	}

	if attrs := buildErrorMessageAttr("msg"); len(attrs) != 1 {
		t.Fatalf("non-empty message should emit attribute, got %#v", attrs)
	}
}

// TestPruneEmptyMapsCoversStringMaps ensures map[string]string entries are pruned.
func TestPruneEmptyMapsCoversStringMaps(t *testing.T) {
	payload := map[string]any{
		"labels": map[string]string{},
		"group": map[string]any{
			"keep": map[string]string{"k": "v"},
			"drop": map[string]string{},
		},
	}

	pruneEmptyMaps(payload)

	if _, exists := payload["labels"]; exists {
		t.Fatalf("empty string map labels should be pruned: %#v", payload)
	}
	group, ok := payload["group"].(map[string]any)
	if !ok {
		t.Fatalf("group map missing: %#v", payload)
	}
	if _, exists := group["drop"]; exists {
		t.Fatalf("empty nested string map not pruned: %#v", group)
	}
	if keep, ok := group["keep"].(map[string]string); !ok || keep["k"] != "v" {
		t.Fatalf("non-empty string map removed unexpectedly: %#v", group["keep"])
	}
}

// TestBuildReportLocationAttrsIncludesFields covers reportLocation assembly with populated frames.
func TestBuildReportLocationAttrsIncludesFields(t *testing.T) {
	frame := runtime.Frame{File: "file.go", Line: 42, Function: "fn"}
	attrs := buildReportLocationAttrs(frame)
	if len(attrs) != 1 {
		t.Fatalf("expected single attribute, got %d", len(attrs))
	}
	ctx, ok := attrs[0].Value.Any().(map[string]any)
	if !ok {
		t.Fatalf("context attribute unexpected type: %#v", attrs[0].Value.Any())
	}
	report, ok := ctx["reportLocation"].(map[string]any)
	if !ok {
		t.Fatalf("reportLocation missing: %#v", ctx)
	}
	if report["filePath"] != "file.go" || report["lineNumber"] != 42 || report["functionName"] != "fn" {
		t.Fatalf("reportLocation contents unexpected: %#v", report)
	}
}

// init exercises fallback resolver hooks before tests run.
func init() {
	_ = getRecordSourceFunc()(slog.Record{})
	_ = getRuntimeFrameResolver()(0)
}

// TestJSONHandlerResolveSourceLocation exercises the enabled/disabled branches.
func TestJSONHandlerResolveSourceLocation(t *testing.T) {
	baseLogger := slog.New(slog.DiscardHandler)

	disabled := newJSONHandler(&handlerConfig{
		AddSource: false,
		Writer:    io.Discard,
	}, slog.LevelInfo, baseLogger)

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "event", 0)
	if loc := disabled.resolveSourceLocation(record); loc != nil {
		t.Fatalf("expected nil source location when AddSource disabled")
	}

	pc, _, _, _ := runtime.Caller(0)
	record = slog.NewRecord(time.Now(), slog.LevelInfo, "event", pc)

	enabled := newJSONHandler(&handlerConfig{
		AddSource: true,
		Writer:    io.Discard,
	}, slog.LevelInfo, baseLogger)

	loc := enabled.resolveSourceLocation(record)
	if loc == nil {
		t.Fatalf("expected source location when AddSource enabled")
	}
	if loc.File == "" || loc.Function == "" || loc.Line == 0 {
		t.Fatalf("incomplete source location: %+v", loc)
	}

	fallback := slog.NewRecord(time.Now(), slog.LevelInfo, "fallback", pc)
	if src := fallback.Source(); src != nil {
		// Force the handler down the runtime.CallersFrames path.
		src.File = ""
		src.Function = ""
	}
	if loc := enabled.resolveSourceLocation(fallback); loc == nil {
		t.Fatalf("expected fallback source location when Source lacks metadata")
	}

	missingPC := slog.NewRecord(time.Now(), slog.LevelInfo, "missing-pc", 0)
	if loc := enabled.resolveSourceLocation(missingPC); loc != nil {
		t.Fatalf("expected nil source location when PC is zero, got %+v", loc)
	}

	origSourceFunc := getRecordSourceFunc()
	origFrameResolver := getRuntimeFrameResolver()
	t.Cleanup(func() {
		setRecordSourceFunc(origSourceFunc)
		setRuntimeFrameResolver(origFrameResolver)
	})

	setRecordSourceFunc(func(slog.Record) *slog.Source {
		return &slog.Source{}
	})
	setRuntimeFrameResolver(func(uintptr) runtime.Frame {
		return runtime.Frame{
			File:     "fallback.go",
			Line:     123,
			Function: "fallback",
		}
	})
	fallbackRecord := slog.NewRecord(time.Now(), slog.LevelInfo, "needs-fallback", pc)
	if loc := enabled.resolveSourceLocation(fallbackRecord); loc == nil || loc.Function != "fallback" {
		t.Fatalf("expected fallback data, got %+v", loc)
	}

	setRecordSourceFunc(func(slog.Record) *slog.Source { return &slog.Source{} })
	setRuntimeFrameResolver(func(uintptr) runtime.Frame { return runtime.Frame{} })
	if loc := enabled.resolveSourceLocation(fallbackRecord); loc != nil {
		t.Fatalf("expected nil when frame lacks metadata, got %+v", loc)
	}
}

// TestJSONHandlerResolveSourceLocationUsesDefaultFrameResolver ensures the default runtimeFrameResolver is exercised.
func TestJSONHandlerResolveSourceLocationUsesDefaultFrameResolver(t *testing.T) {
	baseLogger := slog.New(slog.DiscardHandler)
	handler := newJSONHandler(&handlerConfig{
		AddSource: true,
		Writer:    io.Discard,
	}, slog.LevelInfo, baseLogger)

	pc, _, _, _ := runtime.Caller(0)
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "default-frame", pc)

	orig := getRecordSourceFunc()
	t.Cleanup(func() { setRecordSourceFunc(orig) })
	setRecordSourceFunc(func(slog.Record) *slog.Source {
		return nil
	})

	loc := handler.resolveSourceLocation(record)
	if loc == nil {
		t.Fatal("expected fallback source location when record Source is unavailable")
	}
	if loc.Function == "" || !strings.Contains(loc.Function, "TestJSONHandlerResolveSourceLocationUsesDefaultFrameResolver") {
		t.Fatalf("unexpected source location: %+v", loc)
	}
	if loc.Line == 0 || loc.File == "" {
		t.Fatalf("expected populated file/line in fallback source location: %+v", loc)
	}
}

// TestSourceAndFrameResolversDefaultPaths exercises fallback branches for resolver hooks.
func TestSourceAndFrameResolversDefaultPaths(t *testing.T) {
	origSource := getRecordSourceFunc()
	origFrame := getRuntimeFrameResolver()
	t.Cleanup(func() {
		setRecordSourceFunc(origSource)
		setRuntimeFrameResolver(origFrame)
	})

	setRecordSourceFunc(nil)
	if fn := getRecordSourceFunc(); fn == nil {
		t.Fatalf("getRecordSourceFunc returned nil after nil setter")
	} else if fn(slog.Record{}) != nil {
		t.Fatalf("expected nil source for empty record")
	}

	setRuntimeFrameResolver(nil)
	if fn := getRuntimeFrameResolver(); fn == nil {
		t.Fatalf("getRuntimeFrameResolver returned nil after nil setter")
	} else {
		_ = fn(0)
	}
}

// TestNewJSONHandlerInitialStateFromConfig ensures initial attrs/groups and default leveler are honored.
func TestNewJSONHandlerInitialStateFromConfig(t *testing.T) {
	t.Parallel()

	cfg := &handlerConfig{
		Writer: io.Discard,
		InitialGroupedAttrs: []groupedAttr{
			{groups: []string{"service", "component"}, attr: slog.String("env", "prod")},
			{groups: []string{"service", "component"}, attr: slog.Attr{}},
			{groups: []string{"service", "component"}, attr: slog.Attr{Key: "", Value: slog.AnyValue(nil)}},
			{groups: []string{"service", "component"}, attr: slog.Attr{Key: "version", Value: slog.IntValue(7)}},
		},
		InitialGroups: []string{"service", "component"},
	}
	handler := newJSONHandler(cfg, nil, slog.New(slog.DiscardHandler))

	if handler.leveler == nil {
		t.Fatalf("expected default leveler when none provided")
	}
	if got := handler.leveler.Level(); got != slog.LevelInfo {
		t.Fatalf("leveler.Level() = %v, want %v", got, slog.LevelInfo)
	}
	if len(handler.groups) != 2 {
		t.Fatalf("groups len = %d, want 2", len(handler.groups))
	}
	if handler.groups[0] != "service" || handler.groups[1] != "component" {
		t.Fatalf("groups = %v, want %v", handler.groups, []string{"service", "component"})
	}
	if got := len(handler.groupedAttrs); got != 2 {
		t.Fatalf("groupedAttrs len = %d, want 2", got)
	}
	if handler.groupedAttrs[0].attr.Key != "env" {
		t.Fatalf("first initial attr = %v, want key env", handler.groupedAttrs[0].attr)
	}
	if handler.groupedAttrs[1].attr.Key != "version" || handler.groupedAttrs[1].attr.Value.Int64() != 7 {
		t.Fatalf("second initial attr = %v, want version=7", handler.groupedAttrs[1].attr)
	}

	// Mutating the original config should not affect the handler's copies.
	cfg.InitialGroups[0] = "mutated"
	cfg.InitialGroupedAttrs[0].attr = slog.String("env", "stage")
	if len(cfg.InitialGroupedAttrs[0].groups) > 0 {
		cfg.InitialGroupedAttrs[0].groups[0] = "mutated"
	}

	if handler.groups[0] != "service" {
		t.Fatalf("groups mutated unexpectedly: %v", handler.groups)
	}
	if handler.groupedAttrs[0].attr.Value.String() != "prod" {
		t.Fatalf("initial attr value mutated, got %v", handler.groupedAttrs[0].attr.Value)
	}
	if handler.groupedAttrs[0].groups[0] != "service" {
		t.Fatalf("group association mutated, got %v", handler.groupedAttrs[0].groups)
	}
}

// TestNewJSONHandlerFallbackInitialAttrs exercises the InitialAttrs path when grouped attrs are absent.
func TestNewJSONHandlerFallbackInitialAttrs(t *testing.T) {
	t.Parallel()

	cfg := &handlerConfig{
		Writer:        io.Discard,
		InitialGroups: []string{"fallback"},
		InitialAttrs: []slog.Attr{
			{},
			{Key: "", Value: slog.AnyValue(nil)},
			slog.String("kept", "v"),
		},
	}
	handler := newJSONHandler(cfg, nil, slog.New(slog.DiscardHandler))

	if len(handler.groups) != 1 || handler.groups[0] != "fallback" {
		t.Fatalf("groups = %v, want [fallback]", handler.groups)
	}
	if len(handler.groupedAttrs) != 1 {
		t.Fatalf("groupedAttrs len = %d, want 1", len(handler.groupedAttrs))
	}
	if handler.groupedAttrs[0].attr.Key != "kept" || handler.groupedAttrs[0].attr.Value.String() != "v" {
		t.Fatalf("grouped attr = %v, want kept=v", handler.groupedAttrs[0].attr)
	}
	cfg.InitialAttrs[2] = slog.String("mutated", "x")
	cfg.InitialGroups[0] = "mutated"
	if handler.groupedAttrs[0].attr.Key != "kept" || handler.groups[0] != "fallback" {
		t.Fatalf("handler state mutated by config changes: groups=%v attrs=%v", handler.groups, handler.groupedAttrs)
	}
}

// TestJSONHandlerWithAttrsReturnsSelf ensures WithAttrs short circuits when attrs are empty.
func TestJSONHandlerWithAttrsReturnsSelf(t *testing.T) {
	t.Parallel()

	base := newJSONHandler(&handlerConfig{
		Writer: io.Discard,
	}, slog.LevelInfo, slog.New(slog.DiscardHandler))

	if got := base.WithAttrs(nil); got != base {
		t.Fatalf("WithAttrs(nil) should return same handler instance")
	}
	if got := base.WithAttrs([]slog.Attr{}); got != base {
		t.Fatalf("WithAttrs(empty) should return same handler instance")
	}
}

// TestJSONHandlerWithGroupAndAttrsExtendState verifies WithGroup/WithAttrs reuse mutexes and isolate state.
func TestJSONHandlerWithGroupAndAttrsExtendState(t *testing.T) {
	t.Parallel()

	cfg := &handlerConfig{Writer: io.Discard}
	base := newJSONHandler(cfg, slog.LevelDebug, slog.New(slog.DiscardHandler))
	base.groups = append(base.groups, "base")
	base.groupedAttrs = append(base.groupedAttrs, groupedAttr{
		groups: []string{"base"},
		attr:   slog.String("existing", "1"),
	})

	if got := base.WithGroup(""); got != base {
		t.Fatalf("WithGroup(\"\") should return same handler instance")
	}

	childHandler, ok := base.WithGroup("child").(*jsonHandler)
	if !ok {
		t.Fatalf("WithGroup did not return *jsonHandler")
	}
	if childHandler == base {
		t.Fatalf("WithGroup should return a new handler when name non-empty")
	}
	if childHandler.mu != base.mu {
		t.Fatalf("child handler should reuse base mutex")
	}
	if len(childHandler.groups) != len(base.groups)+1 {
		t.Fatalf("child groups len = %d, want %d", len(childHandler.groups), len(base.groups)+1)
	}
	if childHandler.groups[len(childHandler.groups)-1] != "child" {
		t.Fatalf("missing appended group, groups = %v", childHandler.groups)
	}
	if len(base.groups) != 1 {
		t.Fatalf("base groups mutated: %v", base.groups)
	}
	if len(childHandler.groupedAttrs) != len(base.groupedAttrs) {
		t.Fatalf("child grouped attrs len = %d, want %d", len(childHandler.groupedAttrs), len(base.groupedAttrs))
	}

	attrHandler, ok := base.WithAttrs([]slog.Attr{slog.String("new", "value")}).(*jsonHandler)
	if !ok {
		t.Fatalf("WithAttrs should return *jsonHandler")
	}
	if attrHandler == base {
		t.Fatalf("WithAttrs should create a new handler")
	}
	if len(attrHandler.groupedAttrs) != len(base.groupedAttrs)+1 {
		t.Fatalf("grouped attr len = %d, want %d", len(attrHandler.groupedAttrs), len(base.groupedAttrs)+1)
	}
	last := attrHandler.groupedAttrs[len(attrHandler.groupedAttrs)-1]
	if len(last.groups) != len(base.groups) {
		t.Fatalf("expected attr groups to mirror base groups, got %v", last.groups)
	}
	if last.attr.Key != "new" || last.attr.Value.String() != "value" {
		t.Fatalf("new attr mismatch: %#v", last.attr)
	}
}

type recordingDiagLogger struct {
	messages []string
}

// Printf records a formatted diagnostic message so tests can assert on warnings.
func (r *recordingDiagLogger) Printf(format string, args ...any) {
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
}

// TestJSONHandlerWarnsOnceWhenProjectUnknown ensures diagnostics fire once per process.
func TestJSONHandlerWarnsOnceWhenProjectUnknown(t *testing.T) {
	var diag recordingDiagLogger
	origLogger := traceDiagnosticLogger
	traceDiagnosticLogger = &diag
	t.Cleanup(func() { traceDiagnosticLogger = origLogger })

	var buf bytes.Buffer
	cfg := &handlerConfig{
		Writer:                &buf,
		traceDiagnosticsState: newTraceDiagnostics(TraceDiagnosticsWarnOnce),
	}
	handler := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))

	traceID, _ := trace.TraceIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	spanID, _ := trace.SpanIDFromHex("bbbbbbbbbbbbbbbb")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "missing-project", 0)
	if err := handler.Handle(ctx, record); err != nil {
		t.Fatalf("Handle() returned %v", err)
	}

	entry := decodeJSONLine(t, buf.String())
	if _, exists := entry[TraceKey]; exists {
		t.Fatalf("expected trace field to be omitted when project unknown: %v", entry)
	}
	if got := entry["otel.trace_id"]; got != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("otel.trace_id = %v, want trace id", got)
	}
	if len(diag.messages) != 1 {
		t.Fatalf("expected single diagnostic message, got %d (%v)", len(diag.messages), diag.messages)
	}

	buf.Reset()
	if err := handler.Handle(ctx, record); err != nil {
		t.Fatalf("Handle() second call returned %v", err)
	}
	if len(diag.messages) != 1 {
		t.Fatalf("diagnostics should warn once, got %d", len(diag.messages))
	}
}

// TestJSONHandlerAutoformatTraceFallback ensures raw trace IDs populate Cloud Logging keys when autoformat is allowed.
func TestJSONHandlerAutoformatTraceFallback(t *testing.T) {
	var diag recordingDiagLogger
	origLogger := traceDiagnosticLogger
	traceDiagnosticLogger = &diag
	t.Cleanup(func() { traceDiagnosticLogger = origLogger })

	var buf bytes.Buffer
	cfg := &handlerConfig{
		Writer:                &buf,
		traceAllowAutoformat:  true,
		traceDiagnosticsState: newTraceDiagnostics(TraceDiagnosticsWarnOnce),
	}
	handler := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))

	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "autoformat", 0)
	if err := handler.Handle(ctx, record); err != nil {
		t.Fatalf("Handle() returned %v", err)
	}

	entry := decodeJSONLine(t, buf.String())
	if got := entry[TraceKey]; got != "105445aa7843bc8bf206b12000100000" {
		t.Fatalf("trace field = %v, want raw trace id", got)
	}
	if got := entry[SpanKey]; got != "09158d8185d3c3af" {
		t.Fatalf("spanId = %v, want span id", got)
	}
	if got := entry[SampledKey]; got != true {
		t.Fatalf("trace_sampled = %v, want true", got)
	}
	if _, exists := entry["otel.trace_id"]; exists {
		t.Fatalf("otel.trace_id should be omitted when Cloud Logging fields are emitted: %v", entry)
	}
	if len(diag.messages) != 0 {
		t.Fatalf("autoformat path should not emit diagnostics, got %d", len(diag.messages))
	}
}

// decodeJSONLine unpacks the final JSON line written by the handler for assertions.
func decodeJSONLine(t *testing.T, raw string) map[string]any {
	t.Helper()
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		t.Fatalf("no log output to decode")
	}
	lines := strings.Split(trimmed, "\n")
	line := strings.TrimSpace(lines[len(lines)-1])
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("json.Unmarshal() returned %v", err)
	}
	return entry
}

// TestJSONHandlerHandleSkipsDisabledLevels ensures Handle short-circuits below the threshold.
func TestJSONHandlerHandleSkipsDisabledLevels(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := &handlerConfig{
		Level:  slog.LevelWarn,
		Writer: &buf,
	}
	levelVar := new(slog.LevelVar)
	levelVar.Set(slog.LevelWarn)

	handler := newJSONHandler(cfg, levelVar, slog.New(slog.DiscardHandler))

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "ignored", 0)
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle() returned %v, want nil", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no output when level disabled, got %q", buf.String())
	}
}

// TestPruneEmptyMapsRemovesEntries ensures empty nested maps vanish from payloads.
func TestPruneEmptyMapsRemovesEntries(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"request": map[string]any{
			"keep": map[string]any{"id": 42},
			"drop": map[string]any{},
		},
		"logging.googleapis.com/labels": map[string]string{},
	}

	pruneEmptyMaps(payload)

	if _, ok := payload["logging.googleapis.com/labels"]; ok {
		t.Fatalf("expected empty labels map to be pruned: %v", payload)
	}

	request, ok := payload["request"].(map[string]any)
	if !ok {
		t.Fatalf("request group missing: %v", payload)
	}
	if _, ok := request["drop"]; ok {
		t.Fatalf("empty nested map was not pruned: %v", request)
	}
	if _, ok := request["keep"]; !ok {
		t.Fatalf("non-empty nested map removed unexpectedly: %v", request)
	}
}

// TestPayloadStatePrepareResetsUsedMaps ensures prepare clears root maps and usage tracking.
func TestPayloadStatePrepareResetsUsedMaps(t *testing.T) {
	t.Parallel()

	var ps payloadState
	root := ps.prepare(1)
	root["foo"] = "bar"

	aux := ps.borrowMap(2)
	aux["scratch"] = 1
	if len(ps.usedMaps) != 1 {
		t.Fatalf("usedMaps = %d, want 1", len(ps.usedMaps))
	}

	root = ps.prepare(8)
	if len(root) != 0 {
		t.Fatalf("prepare did not clear root map: %v", root)
	}
	if len(ps.usedMaps) != 0 {
		t.Fatalf("usedMaps = %d after prepare, want 0", len(ps.usedMaps))
	}
}

// TestPayloadStateObtainLabelsClears verifies the labels map is reused and cleared.
func TestPayloadStateObtainLabelsClears(t *testing.T) {
	t.Parallel()

	var ps payloadState
	labels := ps.obtainLabels()
	labels["env"] = "prod"

	reused := ps.obtainLabels()
	if len(reused) != 0 {
		t.Fatalf("labels not cleared, len=%d", len(reused))
	}
	if _, exists := reused["env"]; exists {
		t.Fatalf("stale label entries persisted: %v", reused)
	}
}

// TestPayloadStateBorrowMapRecycle exercises both allocation paths and recycling.
func TestPayloadStateBorrowMapRecycle(t *testing.T) {
	t.Parallel()

	var ps payloadState
	first := ps.borrowMap(1)
	first["alpha"] = "a"
	second := ps.borrowMap(3)
	second["beta"] = "b"

	if len(ps.usedMaps) != 2 {
		t.Fatalf("usedMaps = %d, want 2", len(ps.usedMaps))
	}
	if len(ps.freeMaps) != 0 {
		t.Fatalf("freeMaps = %d before recycle, want 0", len(ps.freeMaps))
	}

	ps.recycle()
	if len(ps.usedMaps) != 0 {
		t.Fatalf("usedMaps = %d after recycle, want 0", len(ps.usedMaps))
	}
	if len(ps.freeMaps) != 2 {
		t.Fatalf("freeMaps = %d after recycle, want 2", len(ps.freeMaps))
	}

	reused := ps.borrowMap(1)
	if len(reused) != 0 {
		t.Fatalf("reused map was not cleared: %v", reused)
	}
	if len(ps.freeMaps) != 1 {
		t.Fatalf("freeMaps = %d after borrowing recycled map, want 1", len(ps.freeMaps))
	}
}

// TestPayloadBuilderHTTPRequestAndServiceContext covers HTTP attr handling and runtime service context injection.
func TestPayloadBuilderHTTPRequestAndServiceContext(t *testing.T) {
	t.Parallel()

	state := &payloadState{}
	pb := &payloadBuilder{
		state:   state,
		payload: make(map[string]any),
		handler: &jsonHandler{cfg: &handlerConfig{}},
	}

	pb.httpReq = &HTTPRequest{}
	if handled := pb.handleHTTPRequestAttr(slog.StringValue("noop")); handled {
		t.Fatalf("handleHTTPRequestAttr should return false when request already set")
	}

	pb.httpReq = nil
	if handled := pb.handleHTTPRequestAttr(slog.StringValue("noop")); handled {
		t.Fatalf("handleHTTPRequestAttr should return false for non-http values")
	}

	req := &HTTPRequest{RequestMethod: http.MethodGet}
	if handled := pb.handleHTTPRequestAttr(slog.AnyValue(req)); !handled || pb.httpReq == nil {
		t.Fatalf("handleHTTPRequestAttr did not capture HTTPRequest value")
	}

	handler := &jsonHandler{
		cfg: &handlerConfig{
			runtimeServiceContext: map[string]string{"service": "checkout"},
		},
	}

	payload := map[string]any{"serviceContext": map[string]any{"service": "existing"}}
	handler.applyServiceContext(payload)
	if svc := payload["serviceContext"].(map[string]any)["service"]; svc != "existing" {
		t.Fatalf("existing serviceContext should be preserved, got %v", svc)
	}

	payload = map[string]any{}
	handler.applyServiceContext(payload)
	ctx, ok := payload["serviceContext"].(map[string]any)
	if !ok || ctx["service"] != "checkout" {
		t.Fatalf("runtime serviceContext was not applied: %#v", payload["serviceContext"])
	}
}

// TestJSONHandlerHandleEmitsLabelsAndServiceContext ensures record labels are emitted alongside
// service context fields and error metadata.
func TestJSONHandlerHandleEmitsLabelsAndServiceContext(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := &handlerConfig{
		Level:             slog.LevelInfo,
		StackTraceEnabled: true,
		StackTraceLevel:   slog.LevelInfo,
		Writer:            &buf,
		runtimeServiceContext: map[string]string{
			"service": "checkout",
			"version": "v1",
		},
	}

	internalLogger := slog.New(slog.DiscardHandler)
	levelVar := new(slog.LevelVar)
	levelVar.Set(slog.LevelInfo)
	handler := newJSONHandler(cfg, levelVar, internalLogger)

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "order created", 0)
	record.AddAttrs(
		slog.String("component", "worker"),
		slog.Any("error", errors.New("flush failed")),
		slog.Group(LabelsGroup,
			slog.String("env", "prod"),
			slog.String("region", "dynamic"),
		),
	)

	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle returned %v", err)
	}

	entry := decodeJSONEntry(t, &buf)

	labels, ok := entry[LabelsGroup].(map[string]any)
	if !ok {
		t.Fatalf("labels missing or wrong type: %T", entry[LabelsGroup])
	}
	if labels["env"] != "prod" {
		t.Fatalf("env label = %v, want %q", labels["env"], "prod")
	}
	if labels["region"] != "dynamic" {
		t.Fatalf("region label = %v, want %q", labels["region"], "dynamic")
	}
	if _, ok := labels["role"]; ok {
		t.Fatalf("unexpected runtime label in payload: %#v", labels)
	}

	if msg, _ := entry[messageKey].(string); msg != "order created: flush failed" {
		t.Fatalf("message = %q, want %q", msg, "order created: flush failed")
	}
	wantType := errors.New("x")
	if got := entry["error_type"]; got != fmt.Sprintf("%T", wantType) {
		t.Fatalf("error_type = %v, want %T", got, wantType)
	}
	if stack, _ := entry[stackTraceKey].(string); stack == "" {
		t.Fatalf("stack_trace missing in entry: %#v", entry)
	}

	serviceContext, ok := entry["serviceContext"].(map[string]any)
	if !ok {
		t.Fatalf("serviceContext missing or wrong type: %T", entry["serviceContext"])
	}
	if serviceContext["service"] != "checkout" || serviceContext["version"] != "v1" {
		t.Fatalf("serviceContext mismatch: %#v", serviceContext)
	}
}

// TestJSONHandlerHandleUsesRecordLabelsOnly ensures record-supplied labels are emitted.
func TestJSONHandlerHandleUsesRecordLabelsOnly(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := &handlerConfig{
		Level:             slog.LevelInfo,
		StackTraceEnabled: true,
		StackTraceLevel:   slog.LevelInfo,
		Writer:            &buf,
	}
	internalLogger := slog.New(slog.DiscardHandler)
	levelVar := new(slog.LevelVar)
	levelVar.Set(slog.LevelInfo)
	handler := newJSONHandler(cfg, levelVar, internalLogger)

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "order created", 0)
	record.AddAttrs(
		slog.Group(LabelsGroup,
			slog.String("env", "prod"),
			slog.String("region", "dynamic"),
		),
	)

	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle returned %v", err)
	}

	entry := decodeJSONEntry(t, &buf)
	labels, ok := entry[LabelsGroup].(map[string]any)
	if !ok {
		t.Fatalf("labels missing or wrong type: %T", entry[LabelsGroup])
	}
	if len(labels) != 2 || labels["env"] != "prod" || labels["region"] != "dynamic" {
		t.Fatalf("labels = %#v, expected only dynamic labels", labels)
	}
}

// TestJSONHandlerHandleOmitsLabelsWithoutRecordLabels ensures labels are omitted when none are supplied.
func TestJSONHandlerHandleOmitsLabelsWithoutRecordLabels(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := &handlerConfig{
		Level:  slog.LevelInfo,
		Writer: &buf,
	}
	levelVar := new(slog.LevelVar)
	levelVar.Set(slog.LevelInfo)
	handler := newJSONHandler(cfg, levelVar, slog.New(slog.DiscardHandler))

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "billing started", 0)
	record.AddAttrs(slog.String("component", "worker"))

	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle() returned %v", err)
	}

	entry := decodeJSONEntry(t, &buf)
	if _, ok := entry[LabelsGroup]; ok {
		t.Fatalf("labels should be omitted when none are supplied, got %#v", entry[LabelsGroup])
	}
	if component, _ := entry["component"].(string); component != "worker" {
		t.Fatalf("component attr not preserved: %v", component)
	}
}

// TestJSONHandlerBuildPayloadNestedGroups ensures nested slog.Group attrs flatten predictably.
func TestJSONHandlerBuildPayloadNestedGroups(t *testing.T) {
	t.Parallel()

	cfg := &handlerConfig{Writer: io.Discard}
	handler := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "nested", 0)
	record.AddAttrs(
		slog.Group("outer",
			slog.String("child", "value"),
			slog.Group("inner", slog.Int("depth", 2)),
		),
		slog.String("root", "present"),
	)

	state := &payloadState{}
	t.Cleanup(state.recycle)
	payload, _, _, _, _, dynamicLabels := handler.buildPayload(record, state)

	if len(dynamicLabels) != 0 {
		t.Fatalf("expected no labels, got %#v", dynamicLabels)
	}
	outer, ok := payload["outer"].(map[string]any)
	if !ok {
		t.Fatalf("outer group missing or wrong type: %T", payload["outer"])
	}
	if got := outer["child"]; got != "value" {
		t.Fatalf("outer[child] = %v, want %q", got, "value")
	}
	inner, ok := outer["inner"].(map[string]any)
	if !ok {
		t.Fatalf("inner group missing or wrong type: %T", outer["inner"])
	}
	if got := inner["depth"]; got != int64(2) {
		t.Fatalf("inner[depth] = %v, want 2", got)
	}
	if got := payload["root"]; got != "present" {
		t.Fatalf("root attr missing: %v", payload)
	}
}

// TestJSONHandlerBuildPayloadInitialLabelsGroup ensures initial label groups route attrs into dynamic labels.
func TestJSONHandlerBuildPayloadInitialLabelsGroup(t *testing.T) {
	t.Parallel()

	cfg := &handlerConfig{
		Writer:        io.Discard,
		InitialGroups: []string{LabelsGroup},
	}
	handler := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "labels", 0)
	record.AddAttrs(
		slog.String("region", "us-central1"),
		slog.String("role", "ingress"),
	)

	state := &payloadState{}
	t.Cleanup(state.recycle)
	payload, _, _, _, _, dynamicLabels := handler.buildPayload(record, state)

	if len(dynamicLabels) != 2 {
		t.Fatalf("expected two labels, got %#v", dynamicLabels)
	}
	if dynamicLabels["region"] != "us-central1" || dynamicLabels["role"] != "ingress" {
		t.Fatalf("dynamic labels mismatch: %#v", dynamicLabels)
	}
	if _, exists := payload["region"]; exists {
		t.Fatalf("region should not exist in payload: %#v", payload)
	}
	if _, exists := payload["role"]; exists {
		t.Fatalf("role should not exist in payload: %#v", payload)
	}
}

// TestJSONHandlerBuildPayloadSkipsEmptyKeysAndNilValues verifies attr filtering and empty initial groups are handled.
func TestJSONHandlerBuildPayloadSkipsEmptyKeysAndNilValues(t *testing.T) {
	t.Parallel()

	cfg := &handlerConfig{
		Writer:        io.Discard,
		InitialGroups: []string{""},
	}
	handler := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "payload", 0)
	record.AddAttrs(
		slog.String("", "ignored"),
		slog.Any("nil_value", nil),
		slog.String("kept", "value"),
	)

	state := &payloadState{}
	t.Cleanup(state.recycle)
	payload, _, _, _, _, dynamicLabels := handler.buildPayload(record, state)

	if _, exists := payload[""]; exists {
		t.Fatalf("empty attr key should be ignored, payload: %#v", payload)
	}
	if _, exists := payload["nil_value"]; exists {
		t.Fatalf("nil attr values should be skipped, payload: %#v", payload)
	}
	if got := payload["kept"]; got != "value" {
		t.Fatalf("payload[kept] = %v, want %q", got, "value")
	}
	if len(dynamicLabels) != 0 {
		t.Fatalf("expected no dynamic labels, got %#v", dynamicLabels)
	}
}

// TestJSONHandlerCapturesErrorAttr ensures error-valued attrs populate reporting metadata.
func TestJSONHandlerCapturesErrorAttr(t *testing.T) {
	t.Parallel()

	cfg := &handlerConfig{
		Writer:            io.Discard,
		StackTraceEnabled: true,
		StackTraceLevel:   slog.LevelError,
	}
	handler := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))

	record := slog.NewRecord(time.Now(), slog.LevelError, "boom", 0)
	record.AddAttrs(slog.Any("failure", errors.New("kaboom")))

	state := &payloadState{}
	t.Cleanup(state.recycle)
	_, _, errType, errMsg, stackStr, _ := handler.buildPayload(record, state)

	if errType == "" || !strings.Contains(errType, "errorString") {
		t.Fatalf("errType = %q, want non-empty error type", errType)
	}
	if errMsg != "kaboom" {
		t.Fatalf("errMsg = %q, want kaboom", errMsg)
	}
	if stackStr == "" {
		t.Fatalf("expected stack trace to be captured for error records")
	}
}

// TestJSONHandlerEmitJSONReturnsWriterError exercises the no-writer guard.
func TestJSONHandlerEmitJSONReturnsWriterError(t *testing.T) {
	t.Parallel()

	handler := newJSONHandler(&handlerConfig{
		Writer: io.Discard,
	}, slog.LevelInfo, slog.New(slog.DiscardHandler))
	handler.writer = nil

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "missing writer", 0)
	if err := handler.emitJSON(record, map[string]any{}, nil, nil, "", "", "", false, false, "", "", ""); err == nil {
		t.Fatal("emitJSON returned nil error without configured writer")
	}
}

// TestJSONHandlerEmitJSONUsesServiceContextAny ensures serviceContextAny is forwarded without buffer pooling.
func TestJSONHandlerEmitJSONUsesServiceContextAny(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := &handlerConfig{
		Writer: &buf,
		runtimeServiceContextAny: map[string]any{
			"service": "checkout",
			"version": "v2",
		},
		runtimeServiceContext: map[string]string{
			"service": "checkout",
		},
	}
	handler := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))
	handler.bufferPool = nil

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "service context", 0)
	payload := map[string]any{}
	if err := handler.emitJSON(record, payload, nil, nil, "", "", "", false, false, "", "", ""); err != nil {
		t.Fatalf("emitJSON returned %v", err)
	}

	entry := decodeJSONEntry(t, &buf)
	ctx, ok := entry["serviceContext"].(map[string]any)
	if !ok {
		t.Fatalf("serviceContext missing or wrong type: %T", entry["serviceContext"])
	}
	if ctx["service"] != "checkout" || ctx["version"] != "v2" {
		t.Fatalf("serviceContext = %#v, want service=checkout version=v2", ctx)
	}
}

// decodeJSONEntry unmarshals newline-delimited JSON payloads emitted by the handler.
func decodeJSONEntry(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	// Handler emits newline-delimited JSON entries. Decode the first
	// non-empty line to avoid failing when the buffer contains trailing
	// newlines or multiple entries (tests that expect a single entry
	// still work).
	data := buf.String()
	lines := strings.Split(data, "\n")
	var first string
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			first = ln
			break
		}
	}
	if first == "" {
		t.Fatalf("expected handler output")
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(first), &entry); err != nil {
		t.Fatalf("json.Unmarshal returned %v; line=%q", err, first)
	}
	return entry
}

// TestJSONHandler_ReplaceAttr_InGroup verifies that ReplaceAttr is correctly invoked for attributes within groups.
func TestJSONHandler_ReplaceAttr_InGroup(t *testing.T) {
	t.Parallel()

	// Ensure ReplaceAttr is called for attributes within groups,
	// and that the groups argument is correctly populated.
	replace := func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) > 0 && groups[0] == "g" && a.Key == "k" {
			return slog.String("k", "replaced")
		}
		return a
	}
	var buf bytes.Buffer
	h := newJSONHandler(&handlerConfig{ReplaceAttr: replace, Writer: &buf}, slog.LevelInfo, nil)
	logger := slog.New(h).WithGroup("g")

	// Use explicit slog.Attr to avoid relying on key/value inference.
	logger.Info("msg", slog.String("k", "v"))

	entry := decodeJSONEntry(t, &buf)
	group, ok := entry["g"].(map[string]any)
	if !ok {
		t.Fatalf("expected group 'g' in output")
	}
	if group["k"] != "replaced" {
		t.Errorf("expected 'k' to be 'replaced', got %v", group["k"])
	}
}

// TestJSONHandler_ErrorValue_Attribute verifies that error values in attributes are correctly captured.
func TestJSONHandler_ErrorValue_Attribute(t *testing.T) {
	t.Parallel()

	// Ensure that an error value in an attribute is captured as the first error
	// if one hasn't been captured yet.

	// We need an attribute that is an error.
	err := errors.New("my error")
	var buf bytes.Buffer
	h := newJSONHandler(&handlerConfig{Writer: &buf}, slog.LevelInfo, slog.New(slog.DiscardHandler))
	logger := slog.New(h)

	// This should trigger the error capture logic in walkAttr
	logger.Info("msg", slog.Any("my_err", err))

	entry := decodeJSONEntry(t, &buf)
	msg, ok := entry["message"].(string)
	if !ok {
		t.Fatalf("message type = %T, want string", entry["message"])
	}
	if msg != "msg: my error" {
		t.Fatalf("message = %q, want %q", msg, "msg: my error")
	}
	if got := entry["my_err"]; got != err.Error() {
		t.Fatalf("my_err attr = %v, want %q", got, err.Error())
	}
	if got := entry["error_type"]; got != fmt.Sprintf("%T", err) {
		t.Fatalf("error_type = %v, want %T", got, err)
	}
	wantSeverity := severityString(slog.LevelInfo, false)
	if sev, ok := entry["severity"].(string); !ok || sev != wantSeverity {
		t.Fatalf("severity = %v (type %T), want %s", entry["severity"], entry["severity"], wantSeverity)
	}
	if _, exists := entry[stackTraceKey]; exists {
		t.Fatalf("unexpected stack_trace present when stack traces are disabled: %#v", entry)
	}
}

// TestJSONHandler_LabelsGroup_InBaseAttrs verifies that the special LabelsGroup is handled correctly when present in base attributes.
func TestJSONHandler_LabelsGroup_InBaseAttrs(t *testing.T) {
	t.Parallel()

	// Ensure that the special LabelsGroup is handled correctly when present
	// in the base attributes (e.g. via WithGroup).

	var buf bytes.Buffer
	h := newJSONHandler(&handlerConfig{Writer: &buf}, slog.LevelInfo, nil)
	// Create a logger with the special labels group and some attributes inside it
	logger := slog.New(h).WithGroup(LabelsGroup).With(slog.String("label_key", "label_val"))

	// Log something to trigger processing of baseAttrs
	logger.Info("msg")

	entry := decodeJSONEntry(t, &buf)
	labels, ok := entry[LabelsGroup].(map[string]any)
	if !ok {
		t.Fatalf("expected labels group in output")
	}
	if labels["label_key"] != "label_val" {
		t.Errorf("expected label_key=label_val, got %v", labels["label_key"])
	}
}
