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
	"testing"
	"time"
)

// TestJSONHandlerResolveSourceLocation exercises the enabled/disabled branches.
func TestJSONHandlerResolveSourceLocation(t *testing.T) {
	t.Parallel()

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

	origSourceFunc := recordSourceFunc
	origFrameResolver := runtimeFrameResolver
	t.Cleanup(func() {
		recordSourceFunc = origSourceFunc
		runtimeFrameResolver = origFrameResolver
	})

	recordSourceFunc = func(slog.Record) *slog.Source {
		return &slog.Source{}
	}
	runtimeFrameResolver = func(uintptr) runtime.Frame {
		return runtime.Frame{
			File:     "fallback.go",
			Line:     123,
			Function: "fallback",
		}
	}
	fallbackRecord := slog.NewRecord(time.Now(), slog.LevelInfo, "needs-fallback", pc)
	if loc := enabled.resolveSourceLocation(fallbackRecord); loc == nil || loc.Function != "fallback" {
		t.Fatalf("expected fallback data, got %+v", loc)
	}

	recordSourceFunc = func(slog.Record) *slog.Source { return &slog.Source{} }
	runtimeFrameResolver = func(uintptr) runtime.Frame { return runtime.Frame{} }
	if loc := enabled.resolveSourceLocation(fallbackRecord); loc != nil {
		t.Fatalf("expected nil when frame lacks metadata, got %+v", loc)
	}
}

// TestJSONHandlerResolveSourceLocationUsesDefaultFrameResolver ensures the default runtimeFrameResolver is exercised.
func TestJSONHandlerResolveSourceLocationUsesDefaultFrameResolver(t *testing.T) {
	t.Parallel()

	baseLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := newJSONHandler(&handlerConfig{
		AddSource: true,
		Writer:    io.Discard,
	}, slog.LevelInfo, baseLogger)

	pc, _, _, _ := runtime.Caller(0)
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "default-frame", pc)

	orig := recordSourceFunc
	t.Cleanup(func() { recordSourceFunc = orig })
	recordSourceFunc = func(slog.Record) *slog.Source {
		return nil
	}

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

// TestNewJSONHandlerInitialStateFromConfig ensures initial attrs/groups and default leveler are honored.
func TestNewJSONHandlerInitialStateFromConfig(t *testing.T) {
	t.Parallel()

	cfg := &handlerConfig{
		Writer:        io.Discard,
		InitialGroups: []string{"service", "component"},
		InitialAttrs: []slog.Attr{
			slog.String("env", "prod"),
			{},
			{Key: "", Value: slog.IntValue(7)},
		},
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
	if handler.groupedAttrs[1].attr.Value.Int64() != 7 {
		t.Fatalf("second initial attr value = %v, want 7", handler.groupedAttrs[1].attr.Value)
	}

	// Mutating the original config should not affect the handler's copies.
	cfg.InitialGroups[0] = "mutated"
	cfg.InitialAttrs[0] = slog.String("env", "stage")

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
	payload, _, _, _, _, dynamicLabels := handler.buildPayload(record, state)
	state.recycle()

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

// decodeJSONEntry unmarshals newline-delimited JSON payloads emitted by the handler.
func decodeJSONEntry(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	data := bytes.TrimSpace(buf.Bytes())
	if len(data) == 0 {
		t.Fatalf("expected handler output")
	}
	var entry map[string]any
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("json.Unmarshal returned %v", err)
	}
	return entry
}
