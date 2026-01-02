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

package slogcp

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"testing"
)

// TestErrorReportingAttrsOptions ensures ErrorReportingAttrs honors overrides.
func TestErrorReportingAttrsOptions(t *testing.T) {
	t.Parallel()

	attrs := ErrorReportingAttrs(
		errors.New("boom"),
		WithErrorMessage("override"),
		WithErrorServiceContext("svc", "v1"),
	)
	if len(attrs) == 0 {
		t.Fatal("expected attrs")
	}

	attrMap := attrsToMapReporting(attrs)
	serviceCtx, ok := attrMap["serviceContext"].(map[string]any)
	if !ok {
		t.Fatalf("serviceContext attr missing or wrong type: %T", attrMap["serviceContext"])
	}
	if serviceCtx["service"] != "svc" || serviceCtx["version"] != "v1" {
		t.Fatalf("serviceContext = %#v, want service=svc version=v1", serviceCtx)
	}
	if got := attrMap["message"]; got != "override" {
		t.Fatalf("message attr = %v, want override", got)
	}
	if _, ok := attrMap["stack_trace"].(string); !ok {
		t.Fatalf("stack_trace missing from attrs = %#v", attrMap)
	}
}

// TestReportErrorEmitsStructuredRecord validates ReportError writes metadata.
func TestReportErrorEmitsStructuredRecord(t *testing.T) {
	t.Parallel()

	recorder := &recordingHandler{}
	logger := slog.New(recorder)

	ReportError(
		context.Background(),
		logger,
		errors.New("explode"),
		"failed",
		WithErrorMessage("epic fail"),
	)

	if recorder.lastRecord == nil {
		t.Fatalf("no record captured")
	}
	if recorder.lastRecord.Message != "failed" {
		t.Fatalf("message = %q, want failed", recorder.lastRecord.Message)
	}
	if recorder.lastRecord.Level != slog.LevelError {
		t.Fatalf("level = %v, want error", recorder.lastRecord.Level)
	}

	attrMap := attrsToMapReporting(recorder.attrs)
	if got := attrMap["error"]; got == nil || got.(error).Error() != "explode" {
		t.Fatalf("error attr = %v, want explode", attrMap["error"])
	}
	if attrMap["message"] != "epic fail" {
		t.Fatalf("message attr = %v, want epic fail", attrMap["message"])
	}
}

// TestErrorReportingAttrsNilError returns nil when err is nil.
func TestErrorReportingAttrsNilError(t *testing.T) {
	t.Parallel()

	if attrs := ErrorReportingAttrs(nil); attrs != nil {
		t.Fatalf("ErrorReportingAttrs(nil) = %v, want nil", attrs)
	}
}

// TestReportErrorNilArguments ensures nil logger or error are no-ops.
func TestReportErrorNilArguments(t *testing.T) {
	t.Parallel()

	recorder := &recordingHandler{}
	logger := slog.New(recorder)

	ReportError(context.Background(), nil, errors.New("boom"), "ignored")
	ReportError(context.Background(), logger, nil, "ignored")

	if recorder.lastRecord != nil {
		t.Fatalf("expected no log entries for nil inputs, got %+v", recorder.lastRecord)
	}
}

// attrsToMap converts attributes into a simple map for assertions.
func attrsToMapReporting(attrs []slog.Attr) map[string]any {
	out := make(map[string]any, len(attrs))
	for _, attr := range attrs {
		if attr.Key == "" {
			continue
		}
		out[attr.Key] = attr.Value.Any()
	}
	return out
}

type recordingHandler struct {
	lastRecord *slog.Record
	attrs      []slog.Attr
}

// Enabled always returns true for the recording handler.
func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle captures the last record and its attributes.
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.lastRecord = &slog.Record{}
	*h.lastRecord = r
	h.attrs = make([]slog.Attr, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		h.attrs = append(h.attrs, a)
		return true
	})
	return nil
}

// WithAttrs clones the handler with additional attributes.
func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recordingHandler{lastRecord: h.lastRecord, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

// WithGroup returns the handler unchanged because grouping is unused here.
func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

// TestErrorReportingAttrsUsesRuntimeContext verifies runtime service context data is applied.
func TestErrorReportingAttrsUsesRuntimeContext(t *testing.T) {
	t.Parallel()

	t.Cleanup(resetRuntimeInfoCache)

	stubRuntimeInfo(RuntimeInfo{
		ServiceContext: map[string]string{
			"service": "runtime-svc",
			"version": "runtime-v2",
		},
	})

	attrs := ErrorReportingAttrs(errors.New("kaboom"))
	if len(attrs) == 0 {
		t.Fatalf("ErrorReportingAttrs returned no attributes")
	}

	attrMap := make(map[string]any, len(attrs))
	for _, attr := range attrs {
		if attr.Key == "" {
			continue
		}
		attrMap[attr.Key] = attr.Value.Any()
	}

	serviceValue := attrMap["serviceContext"]
	ctx, ok := serviceValue.(map[string]any)
	if !ok {
		t.Fatalf("serviceContext type = %T, want map[string]any", serviceValue)
	}
	if ctx["service"] != "runtime-svc" || ctx["version"] != "runtime-v2" {
		t.Fatalf("serviceContext = %#v, want runtime service metadata", ctx)
	}

	if _, hasMessage := attrMap["message"]; hasMessage {
		t.Fatalf("unexpected message attribute when override not provided")
	}

	keys := make([]string, 0, len(attrMap))
	for k := range attrMap {
		keys = append(keys, k)
	}
	if _, ok := attrMap["stack_trace"]; !ok {
		t.Fatalf("stack_trace missing from attrs: %v", keys)
	}
}

// TestErrorReportingHelpersHandleEmptyInput covers helper fallbacks when optional data is absent.
func TestErrorReportingHelpersHandleEmptyInput(t *testing.T) {
	t.Parallel()

	if attrs := buildServiceContextAttrs(errorReportingConfig{}); attrs != nil {
		t.Fatalf("buildServiceContextAttrs(empty) = %#v, want nil", attrs)
	}
	if attrs := buildStackAttrs(""); attrs != nil {
		t.Fatalf("buildStackAttrs(empty) = %#v, want nil", attrs)
	}
	if attrs := buildReportLocationAttrs(runtime.Frame{}); attrs != nil {
		t.Fatalf("buildReportLocationAttrs(zero frame) = %#v, want nil", attrs)
	}
	if attrs := buildErrorMessageAttr(""); attrs != nil {
		t.Fatalf("buildErrorMessageAttr(empty) = %#v, want nil", attrs)
	}
}

// TestBuildErrorMessageAttrCoversNonEmpty verifies the non-empty branch.
func TestBuildErrorMessageAttrCoversNonEmpty(t *testing.T) {
	t.Parallel()

	attrs := buildErrorMessageAttr("boom")
	if len(attrs) != 1 || attrs[0].Value.String() != "boom" {
		t.Fatalf("buildErrorMessageAttr returned %#v, want message attr with boom", attrs)
	}
}

// TestBuildErrorReportingConfigSkipsEmptyRuntimeContext ensures runtime info without service context leaves config untouched.
func TestBuildErrorReportingConfigSkipsEmptyRuntimeContext(t *testing.T) {
	t.Parallel()
	t.Cleanup(resetRuntimeInfoCache)

	stubRuntimeInfo(RuntimeInfo{ServiceContext: nil})

	cfg := buildErrorReportingConfig(nil)
	if cfg.service != "" || cfg.version != "" {
		t.Fatalf("expected empty config when runtime service context missing, got %+v", cfg)
	}
}
