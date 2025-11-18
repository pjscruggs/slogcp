package slogcp

import (
	"runtime"
	"strings"
	"testing"
)

// stackError implements the stackTracer interface for exercising extractor helpers.
type stackError struct {
	pcs []uintptr
}

// Error implements the error interface for stackError.
func (s stackError) Error() string { return "stack error" }

// StackTrace implements the stackTracer contract by exposing stored PCs.
func (s stackError) StackTrace() []uintptr { return s.pcs }

// TestExtractAndFormatOriginStackUsesStackTracer ensures stackTracer implementations are honoured.
func TestExtractAndFormatOriginStackUsesStackTracer(t *testing.T) {
	pcs := make([]uintptr, 16)
	n := runtime.Callers(0, pcs)
	err := stackError{pcs: pcs[:n]}

	stack := extractAndFormatOriginStack(err)
	if stack == "" {
		t.Fatalf("extractAndFormatOriginStack returned empty string")
	}
	if !strings.Contains(stack, "TestExtractAndFormatOriginStackUsesStackTracer") {
		t.Fatalf("stack trace missing test function: %s", stack)
	}
}

// TestTrimStackPCsSkipsFrames asserts trimStackPCs honours the provided skip function.
func TestTrimStackPCsSkipsFrames(t *testing.T) {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(0, pcs)
	trimmed := trimStackPCs(pcs[:n], func(name string) bool {
		return !strings.Contains(name, "TestTrimStackPCsSkipsFrames")
	})

	if len(trimmed) == 0 {
		t.Fatalf("trimStackPCs returned no frames")
	}

	frame, _ := runtime.CallersFrames(trimmed).Next()
	if !strings.Contains(frame.Function, "TestTrimStackPCsSkipsFrames") {
		t.Fatalf("unexpected top frame %q", frame.Function)
	}
}
