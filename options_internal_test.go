package slogcp

import (
	"log/slog"
	"testing"
)

// TestWithAttrsSkipsEmptySlices ensures empty slices do not mutate options state.
func TestWithAttrsSkipsEmptySlices(t *testing.T) {
	t.Parallel()

	opt := WithAttrs(nil)
	opts := &options{}
	opt(opts)
	if len(opts.attrs) != 0 {
		t.Fatalf("opts.attrs length = %d, want 0", len(opts.attrs))
	}

	opt = WithAttrs([]slog.Attr{})
	opt(opts)
	if len(opts.attrs) != 0 {
		t.Fatalf("opts.attrs should remain empty for blank slices")
	}
}

// TestWithAttrsCopiesInput verifies the option stores its own copy of the provided attributes.
func TestWithAttrsCopiesInput(t *testing.T) {
	t.Parallel()

	input := []slog.Attr{slog.String("mutable", "before")}
	opts := &options{}
	WithAttrs(input)(opts)

	input[0] = slog.String("mutable", "after")

	if len(opts.attrs) != 1 || len(opts.attrs[0]) != 1 {
		t.Fatalf("opts.attrs not populated as expected: %#v", opts.attrs)
	}
	if got := opts.attrs[0][0].Value.String(); got != "before" {
		t.Fatalf("copied attribute = %q, want %q", got, "before")
	}
}
