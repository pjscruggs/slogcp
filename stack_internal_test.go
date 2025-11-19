package slogcp

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

// fakeStackError exposes a canned stack trace for exercising stackTracer logic.
type fakeStackError struct {
	pcs []uintptr
}

// Error implements the error interface for fakeStackError.
func (f fakeStackError) Error() string { return "fake-stack" }

// StackTrace returns the predetermined program counters.
func (f fakeStackError) StackTrace() []uintptr {
	return f.pcs
}

// captureProgramCounters collects the current stack for use in tests.
func captureProgramCounters(t *testing.T) []uintptr {
	t.Helper()

	pcs := make([]uintptr, maxStackFrames+32)
	n := runtime.Callers(0, pcs)
	if n == 0 {
		t.Fatalf("runtime.Callers() returned 0 frames")
	}
	return pcs[:n]
}

// repeatPCs extends pcs to reach want entries by repeating values.
func repeatPCs(pcs []uintptr, want int) []uintptr {
	out := make([]uintptr, 0, want)
	for len(out) < want {
		remaining := want - len(out)
		if remaining > len(pcs) {
			remaining = len(pcs)
		}
		out = append(out, pcs[:remaining]...)
	}
	return out
}

// TestExtractAndFormatOriginStackUsesTracer ensures stackTracer implementations are honored and capped.
func TestExtractAndFormatOriginStackUsesTracer(t *testing.T) {
	t.Parallel()

	pcs := repeatPCs(captureProgramCounters(t), maxStackFrames+5)
	stack := extractAndFormatOriginStack(fakeStackError{pcs: pcs})
	if stack == "" {
		t.Fatal("expected non-empty stack trace from stackTracer")
	}
	if !strings.Contains(stack, "TestExtractAndFormatOriginStackUsesTracer") {
		t.Fatalf("stack trace missing test function:\n%s", stack)
	}

	lines := strings.Split(strings.TrimSpace(stack), "\n")
	if len(lines) < 3 {
		t.Fatalf("stack too short: %q", stack)
	}
	frameLines := (len(lines) - 1) / 2
	if frameLines > maxStackFrames {
		t.Fatalf("stack included %d frames, want <= %d", frameLines, maxStackFrames)
	}

	if stack := extractAndFormatOriginStack(errors.New("plain")); stack != "" {
		t.Fatalf("expected empty stack for errors without stackTracer, got %q", stack)
	}
}

// TestFormatPCsToStackStringHandlesEmptySlice verifies nil inputs return an empty string.
func TestFormatPCsToStackStringHandlesEmptySlice(t *testing.T) {
	t.Parallel()

	if got := formatPCsToStackString(nil); got != "" {
		t.Fatalf("formatPCsToStackString(nil) = %q, want empty", got)
	}
}

// TestFormatPCsToStackStringSkipsInvalidFrames forces headerless formatting and invalid frames.
func TestFormatPCsToStackStringSkipsInvalidFrames(t *testing.T) {
	origHeader := goroutineHeaderFunc
	origFrames := callersFramesFunc
	t.Cleanup(func() {
		goroutineHeaderFunc = origHeader
		callersFramesFunc = origFrames
	})

	goroutineHeaderFunc = func() string { return "" }
	callersFramesFunc = func([]uintptr) frameIterator {
		return &stubFrameIterator{frames: []runtime.Frame{
			{PC: 1, Function: ""},
			{PC: 2, Function: "runtime.goexit"},
			{PC: 3, Function: "main.main", File: "main.go", Line: 42, Entry: 2},
			{PC: 0},
		}}
	}

	result := formatPCsToStackString([]uintptr{1, 2, 3, 0})
	if result == "" {
		t.Fatalf("expected formatted stack trace without header")
	}
	if strings.Contains(result, "goroutine") {
		t.Fatalf("unexpected goroutine header in %q", result)
	}
	if !strings.Contains(result, "main.main") || !strings.Contains(result, "main.go:42") {
		t.Fatalf("formatted stack missing frame info: %q", result)
	}
}

// TestFormatPCsToStackStringHandlesTrailingEmptyFunction covers the branch where no frames remain.
func TestFormatPCsToStackStringHandlesTrailingEmptyFunction(t *testing.T) {
	origHeader := goroutineHeaderFunc
	origFrames := callersFramesFunc
	t.Cleanup(func() {
		goroutineHeaderFunc = origHeader
		callersFramesFunc = origFrames
	})

	goroutineHeaderFunc = func() string { return "" }
	callersFramesFunc = func([]uintptr) frameIterator {
		return &stubFrameIterator{frames: []runtime.Frame{
			{PC: 1, Function: ""},
		}}
	}

	if got := formatPCsToStackString([]uintptr{1}); got != "" {
		t.Fatalf("formatPCsToStackString() = %q, want empty string for trailing empty frame", got)
	}
}

// TestTrimStackPCsSkipsRuntimeFrames verifies runtime frames fall through and all-skip cases return nil.
func TestTrimStackPCsSkipsRuntimeFrames(t *testing.T) {
	t.Parallel()

	pcs := captureProgramCounters(t)
	trimmed := trimStackPCs(pcs, func(name string) bool {
		return strings.HasPrefix(name, "runtime.")
	})
	if len(trimmed) == len(pcs) {
		t.Fatalf("expected runtime frames to be removed, trimmed len = %d", len(trimmed))
	}
	if len(trimmed) == 0 {
		t.Fatalf("trimmed all frames unexpectedly")
	}

	allTrimmed := trimStackPCs(pcs[:1], func(string) bool { return true })
	if allTrimmed != nil {
		t.Fatalf("expected nil slice when every frame skipped, got %v", allTrimmed)
	}
}

// TestTrimStackPCsHandlesEmptyAndPassthrough ensures empty slices and noop skip functions are preserved.
func TestTrimStackPCsHandlesEmptyAndPassthrough(t *testing.T) {
	t.Parallel()

	t.Run("empty_input", func(t *testing.T) {
		empty := []uintptr{}
		trimmed := trimStackPCs(empty, nil)
		if len(trimmed) != 0 {
			t.Fatalf("trimStackPCs should leave empty slices untouched, got len=%d", len(trimmed))
		}
		if trimmed == nil {
			t.Fatalf("trimStackPCs should preserve zero-length slices instead of returning nil")
		}
	})

	t.Run("no_skips", func(t *testing.T) {
		pcs := captureProgramCounters(t)
		trimmed := trimStackPCs(pcs, func(string) bool { return false })
		if len(trimmed) != len(pcs) {
			t.Fatalf("expected identical slice when skipFn never matches, got %d vs %d", len(trimmed), len(pcs))
		}
		if &trimmed[0] != &pcs[0] {
			t.Fatalf("expected trimStackPCs to reuse input slice when nothing skipped")
		}
	})
}

// TestSkipInternalStackFrameRecognizesPrefixes covers well-known prefixes and user frames.
func TestSkipInternalStackFrameRecognizesPrefixes(t *testing.T) {
	t.Parallel()

	if !SkipInternalStackFrame("runtime.Callers") {
		t.Fatalf("runtime.Callers should be internal")
	}
	if !SkipInternalStackFrame("github.com/pjscruggs/slogcp/json_handler.(*jsonHandler).Handle") {
		t.Fatalf("slogcp prefix should be treated as internal")
	}
	if SkipInternalStackFrame("main.main") {
		t.Fatalf("application frames should not be considered internal")
	}
}

// TestSkipInternalStackFrameBlankInput returns false when funcName is empty to avoid trimming user frames.
func TestSkipInternalStackFrameBlankInput(t *testing.T) {
	t.Parallel()

	if SkipInternalStackFrame("") {
		t.Fatalf("blank function names should not be classified as internal")
	}
}

// TestCaptureStackFallsBackWhenTrimmedEmpty ensures stacks are still emitted when everything is trimmed.
func TestCaptureStackFallsBackWhenTrimmedEmpty(t *testing.T) {
	t.Parallel()

	stack, frame := CaptureStack(func(string) bool { return true })
	if stack == "" {
		t.Fatal("expected stack when skipFn removes every frame")
	}
	if frame.Function == "" {
		t.Fatalf("expected frame metadata even when trimming removes everything")
	}
}

// TestCaptureStackHandlesEmptyRuntimeCallers exercises the path where runtime.Callers returns no PCs.
func TestCaptureStackHandlesEmptyRuntimeCallers(t *testing.T) {
	orig := runtimeCallersFunc
	t.Cleanup(func() { runtimeCallersFunc = orig })
	runtimeCallersFunc = func(int, []uintptr) int { return 0 }

	stack, frame := CaptureStack(nil)
	if stack != "" {
		t.Fatalf("expected empty stack string when runtime.Callers yields nothing, got %q", stack)
	}
	if frame.Function != "" || frame.PC != 0 {
		t.Fatalf("expected zero frame when runtime.Callers yields nothing, got %+v", frame)
	}
}

// TestCurrentGoroutineHeaderIsClean asserts the header contains printable data with no control runes.
func TestCurrentGoroutineHeaderIsClean(t *testing.T) {
	t.Parallel()

	header := currentGoroutineHeader()
	if header == "" {
		t.Fatalf("currentGoroutineHeader returned empty string")
	}
	if strings.Contains(header, "\n") {
		t.Fatalf("goroutine header should not contain newline: %q", header)
	}
	if strings.Contains(header, "\r") {
		t.Fatalf("goroutine header should not contain carriage return: %q", header)
	}
}

// TestCurrentGoroutineHeaderFallback verifies fallback header when runtime.Stack fails.
func TestCurrentGoroutineHeaderFallback(t *testing.T) {
	origStack := runtimeStackFunc
	origHeader := goroutineHeaderFunc
	t.Cleanup(func() {
		runtimeStackFunc = origStack
		goroutineHeaderFunc = origHeader
	})

	runtimeStackFunc = func([]byte, bool) int { return 0 }
	goroutineHeaderFunc = defaultGoroutineHeader

	if header := currentGoroutineHeader(); header != "goroutine 0 [running]:" {
		t.Fatalf("fallback header = %q, want goroutine 0 [running]:", header)
	}
}

// TestDefaultGoroutineHeaderTrimsWhitespace ensures empty headers fall back to the default string.
func TestDefaultGoroutineHeaderTrimsWhitespace(t *testing.T) {
	origStack := runtimeStackFunc
	origHeader := goroutineHeaderFunc
	t.Cleanup(func() {
		runtimeStackFunc = origStack
		goroutineHeaderFunc = origHeader
	})

	runtimeStackFunc = func(buf []byte, _ bool) int {
		copy(buf, "\n\t \r\n")
		return len("\n\t \r\n")
	}
	goroutineHeaderFunc = defaultGoroutineHeader

	if header := currentGoroutineHeader(); header != "goroutine 0 [running]:" {
		t.Fatalf("header = %q, want fallback", header)
	}
}

// stubFrameIterator implements frameIterator for tests.
type stubFrameIterator struct {
	frames []runtime.Frame
	idx    int
}

// Next returns the next runtime.Frame in the stubbed sequence.
func (s *stubFrameIterator) Next() (runtime.Frame, bool) {
	if s.idx >= len(s.frames) {
		return runtime.Frame{}, false
	}
	frame := s.frames[s.idx]
	s.idx++
	return frame, s.idx < len(s.frames)
}
