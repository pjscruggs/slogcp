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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// TestJSONHandlerResolveSourceLocation exercises the enabled/disabled branches.
func TestJSONHandlerResolveSourceLocation(t *testing.T) {
	baseLogger := slog.New(slog.NewTextHandler(io.Discard, nil))

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
	baseLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
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

	recordSourceFunc = atomic.Value{}
	if fn := getRecordSourceFunc(); fn == nil {
		t.Fatalf("getRecordSourceFunc returned nil after zeroing value")
	}

	setRuntimeFrameResolver(nil)
	if fn := getRuntimeFrameResolver(); fn == nil {
		t.Fatalf("getRuntimeFrameResolver returned nil after nil setter")
	} else {
		_ = fn(0)
	}

	runtimeFrameResolver = atomic.Value{}
	if fn := getRuntimeFrameResolver(); fn == nil {
		t.Fatalf("getRuntimeFrameResolver returned nil after zeroing value")
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
	handler := newJSONHandler(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

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
	handler := newJSONHandler(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

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
	}, slog.LevelInfo, slog.New(slog.NewTextHandler(io.Discard, nil)))

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
	base := newJSONHandler(cfg, slog.LevelDebug, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	handler := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.NewTextHandler(io.Discard, nil)))

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
	handler := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.NewTextHandler(io.Discard, nil)))

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

	handler := newJSONHandler(cfg, levelVar, slog.New(slog.NewTextHandler(io.Discard, nil)))

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

// TestJSONHandlerHandleMergesLabels ensures runtime labels are merged with dynamic labels and
// service context fields are emitted alongside error metadata.
func TestJSONHandlerHandleMergesLabels(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := &handlerConfig{
		Level:             slog.LevelInfo,
		StackTraceEnabled: true,
		StackTraceLevel:   slog.LevelInfo,
		Writer:            &buf,
		runtimeLabels:     map[string]string{"role": "api", "region": "static"},
		runtimeServiceContext: map[string]string{
			"service": "checkout",
			"version": "v1",
		},
	}

	internalLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
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
	if labels["role"] != "api" {
		t.Fatalf("role label = %v, want %q", labels["role"], "api")
	}
	if labels["region"] != "dynamic" {
		t.Fatalf("region label = %v, want %q", labels["region"], "dynamic")
	}
	if cfg.runtimeLabels["region"] != "static" {
		t.Fatalf("runtime labels were mutated: %#v", cfg.runtimeLabels)
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

// TestJSONHandlerHandleUsesRuntimeLabelsWithoutRecordLabels covers the runtime label fallback path.
func TestJSONHandlerHandleUsesRuntimeLabelsWithoutRecordLabels(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := &handlerConfig{
		Level:         slog.LevelInfo,
		Writer:        &buf,
		runtimeLabels: map[string]string{"service": "billing"},
	}
	levelVar := new(slog.LevelVar)
	levelVar.Set(slog.LevelInfo)
	handler := newJSONHandler(cfg, levelVar, slog.New(slog.NewTextHandler(io.Discard, nil)))

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "billing started", 0)
	record.AddAttrs(slog.String("component", "worker"))

	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle() returned %v", err)
	}

	entry := decodeJSONEntry(t, &buf)
	labels, ok := entry[LabelsGroup].(map[string]any)
	if !ok {
		t.Fatalf("labels missing or wrong type: %T", entry[LabelsGroup])
	}
	if len(labels) != 1 || labels["service"] != "billing" {
		t.Fatalf("labels = %#v, want only runtime labels", labels)
	}
	if cfg.runtimeLabels["service"] != "billing" {
		t.Fatalf("runtime labels mutated: %#v", cfg.runtimeLabels)
	}
	if component, _ := entry["component"].(string); component != "worker" {
		t.Fatalf("component attr not preserved: %v", component)
	}
}

// TestJSONHandlerBuildPayloadNestedGroups ensures nested slog.Group attrs flatten predictably.
func TestJSONHandlerBuildPayloadNestedGroups(t *testing.T) {
	t.Parallel()

	cfg := &handlerConfig{Writer: io.Discard}
	handler := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.NewTextHandler(io.Discard, nil)))

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
	handler := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.NewTextHandler(io.Discard, nil)))

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
	handler := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.NewTextHandler(io.Discard, nil)))

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
	handler := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.NewTextHandler(io.Discard, nil)))

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
	}, slog.LevelInfo, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	handler := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	h := newJSONHandler(&handlerConfig{Writer: &buf}, slog.LevelInfo, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
