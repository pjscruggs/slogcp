package slogcp_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/pjscruggs/slogcp"
)

// TestErrorReportingAttrsOptions ensures ErrorReportingAttrs honors overrides.
func TestErrorReportingAttrsOptions(t *testing.T) {
	t.Parallel()

	attrs := slogcp.ErrorReportingAttrs(
		errors.New("boom"),
		slogcp.WithErrorMessage("override"),
		slogcp.WithErrorServiceContext("svc", "v1"),
	)
	if len(attrs) == 0 {
		t.Fatal("expected attrs")
	}

	attrMap := attrsToMap(attrs)
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

	slogcp.ReportError(
		context.Background(),
		logger,
		errors.New("explode"),
		"failed",
		slogcp.WithErrorMessage("epic fail"),
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

	attrMap := attrsToMap(recorder.attrs)
	if got := attrMap["error"]; got == nil || got.(error).Error() != "explode" {
		t.Fatalf("error attr = %v, want explode", attrMap["error"])
	}
	if attrMap["message"] != "epic fail" {
		t.Fatalf("message attr = %v, want epic fail", attrMap["message"])
	}
}

// attrsToMap converts attributes into a simple map for assertions.
func attrsToMap(attrs []slog.Attr) map[string]any {
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
