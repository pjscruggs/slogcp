package slogcp

import (
	"io"
	"log/slog"
	"testing"
)

// TestSourceAwareHandlerPropagatesSourceMetadata validates HasSource/With* wrappers.
func TestSourceAwareHandlerPropagatesSourceMetadata(t *testing.T) {
	t.Parallel()

	base := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{AddSource: true})
	wrapped := sourceAwareHandler{Handler: base}

	if !wrapped.HasSource() {
		t.Fatalf("sourceAwareHandler.HasSource() = false, want true")
	}
	if _, ok := wrapped.WithAttrs(nil).(sourceAwareHandler); !ok {
		t.Fatalf("WithAttrs did not return sourceAwareHandler")
	}
	if _, ok := wrapped.WithGroup("grp").(sourceAwareHandler); !ok {
		t.Fatalf("WithGroup did not return sourceAwareHandler")
	}
}
