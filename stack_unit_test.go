//go:build unit
// +build unit

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

package slogcp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/pjscruggs/slogcp"
)

// customErrorWithStackTrace is an error that provides its own stack trace.
// This type is used to test slogcp's handling of errors that implement
// a stack trace provider compatible with its internal stackTracer interface,
// which expects a StackTrace() []uintptr method.
type customErrorWithStackTrace struct {
	msg string
	pcs []uintptr
}

// Error implements the error interface.
func (e *customErrorWithStackTrace) Error() string {
	return e.msg
}

// StackTrace makes customErrorWithStackTrace provide program counters,
// satisfying the expected interface for custom stack trace providers.
func (e *customErrorWithStackTrace) StackTrace() []uintptr {
	return e.pcs
}

// newCustomErrorWithStackTrace creates an error that captures a stack trace
// from the point of its creation. The skipFrames argument adjusts how many
// frames are skipped by runtime.Callers, allowing the captured trace to
// begin at a relevant point for testing.
func newCustomErrorWithStackTrace(msg string, skipFrames int) error {
	var pcs [10]uintptr // Capture up to 10 frames for the test.
	// runtime.Callers skips itself (frame 0) and its direct caller (frame 1).
	// So, 2 + skipFrames starts the capture from the desired point.
	n := runtime.Callers(2+skipFrames, pcs[:])
	return &customErrorWithStackTrace{msg: msg, pcs: pcs[:n]}
}

// wrappedError is a simple error wrapper used for testing how slogcp
// handles stack traces from errors nested within other errors.
type wrappedError struct {
	msg string
	err error // The underlying error.
}

// Error implements the error interface for wrappedError.
func (e *wrappedError) Error() string { return e.msg + ": " + e.err.Error() }

// Unwrap provides compatibility with errors.Is and errors.As by returning
// the underlying error.
func (e *wrappedError) Unwrap() error { return e.err }

// captureStdOutput captures data written to os.Stdout by a given action.
// It redirects os.Stdout to a pipe, executes the action, and then reads
// from the pipe to return the captured output as a string.
// This helper is not safe for concurrent execution by multiple tests
// that modify os.Stdout.
func captureStdOutput(t *testing.T, action func()) string {
	t.Helper() // Marks this function as a test helper.
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("captureStdOutput: Failed to create pipe: %v", err)
	}
	os.Stdout = w

	// Ensure os.Stdout is restored and the pipe writer is closed after the test.
	t.Cleanup(func() {
		os.Stdout = oldStdout
		_ = w.Close() // Signal EOF to the reading goroutine.
	})

	outC := make(chan string) // Channel to receive output from the goroutine.
	go func() {
		var buf bytes.Buffer
		_, copyErr := io.Copy(&buf, r)
		// Ignore os.ErrClosed and io.ErrClosedPipe as these are expected when the writer closes.
		if copyErr != nil && !errors.Is(copyErr, os.ErrClosed) && !errors.Is(copyErr, io.ErrClosedPipe) {
			t.Logf("captureStdOutput: Error copying from pipe reader: %v", copyErr)
		}
		_ = r.Close() // Close the reader end of the pipe.
		outC <- buf.String()
	}()

	action()      // Execute the logging action.
	_ = w.Close() // Explicitly close writer after action to ensure goroutine unblocks.

	return <-outC // Wait for and return the captured output.
}

// parseLogLine unmarshals a single JSON log line from a string into a map.
// It fails the test if the line is not valid JSON.
func parseLogLine(t *testing.T, line string) map[string]interface{} {
	t.Helper()
	var entry map[string]interface{}
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("parseLogLine: Failed to parse JSON log entry '%s': %v", line, err)
	}
	return entry
}

// findLogEntry scans a multi-line string of log output, parses each line as JSON,
// and returns the first log entry that contains a specific substring in its "message" field.
// If no such entry is found, it fails the test.
func findLogEntry(t *testing.T, output string, msgSubstring string) map[string]interface{} {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		// Quick check for substring before full parse for efficiency.
		if strings.Contains(line, msgSubstring) {
			entry := parseLogLine(t, line)
			// Verify the message field specifically.
			if msg, ok := entry["message"].(string); ok && strings.Contains(msg, msgSubstring) {
				return entry
			}
		}
	}
	t.Logf("Output was:\n%s", output) // Log output for debugging if entry not found.
	t.Fatalf("findLogEntry: Log entry containing message substring '%s' not found.", msgSubstring)
	return nil // Should be unreachable due to t.Fatalf.
}

// TestStackTraceHandling verifies the various conditions under which stack traces
// are included or excluded from log entries. It covers fallback stack traces,
// traces from custom error types, the effect of enabling/disabling options,
// and log level thresholds.
func TestStackTraceHandling(t *testing.T) {
	// Subtests here modify global state (os.Stdout via captureStdOutput) if not
	// careful, but captureStdOutput is designed to be called per action.

	t.Run("FallbackStackTraceOnError", func(t *testing.T) {
		const logMsg = "test fallback stack trace"
		output := captureStdOutput(t, func() {
			logger, err := slogcp.New(
				slogcp.WithRedirectToStdout(),
				slogcp.WithStackTraceEnabled(true),
				slogcp.WithStackTraceLevel(slog.LevelError),
			)
			if err != nil {
				t.Fatalf("New() failed: %v", err)
			}
			defer logger.Close()
			logger.ErrorContext(context.Background(), logMsg, slog.Any("error", errors.New("standard error")))
		})

		entry := findLogEntry(t, output, logMsg)
		stack, ok := entry["stack_trace"].(string)
		if !ok {
			t.Errorf("stack_trace field: got none or non-string, want string. Entry: %+v", entry)
		}
		if stack == "" {
			t.Errorf("stack_trace field: got empty, want non-empty fallback stack. Entry: %+v", entry)
		}
		// Check for a common pattern in Go testing stack traces or runtime functions.
		// This is a basic sanity check that it's a Go stack trace.
		if !strings.Contains(stack, "testing.tRunner") && !strings.Contains(stack, "runtime.goexit") && !strings.Contains(stack, "slogcp.") {
			t.Logf("Fallback stack trace content unexpected: %s", stack)
		}
	})

	t.Run("CustomErrorStackTraceProvided", func(t *testing.T) {
		const logMsg = "test custom stack trace"
		const errMsg = "error with custom stack"
		var output string

		// specificTestFunc creates an error whose stack trace should originate here.
		var specificTestFunc = func() error {
			// skipFrames = 0 ensures newCustomErrorWithStackTrace captures its direct caller,
			// which is this anonymous function (specificTestFunc).
			return newCustomErrorWithStackTrace(errMsg, 0)
		}
		customErr := specificTestFunc() // The error now holds PCs pointing to specificTestFunc.

		output = captureStdOutput(t, func() {
			logger, err := slogcp.New(
				slogcp.WithRedirectToStdout(),
				slogcp.WithStackTraceEnabled(true), // Required to process any error's stack.
				slogcp.WithStackTraceLevel(slog.LevelError),
			)
			if err != nil {
				t.Fatalf("New() failed: %v", err)
			}
			defer logger.Close()
			logger.ErrorContext(context.Background(), logMsg, slog.Any("error", customErr))
		})

		entry := findLogEntry(t, output, logMsg)
		stack, ok := entry["stack_trace"].(string)
		if !ok {
			t.Errorf("stack_trace field: got none or non-string, want string. Entry: %+v", entry)
		}
		if stack == "" {
			t.Errorf("stack_trace field: got empty, want non-empty custom stack. Entry: %+v", entry)
		}
		// The stack trace is generated from PCs inside 'customErr'. These PCs point to
		// 'specificTestFunc'. The runtime formats this as 'TestStackTraceHandling.func...'.
		// We verify that the file of this test and this general pattern appear.
		if !strings.Contains(stack, "stack_unit_test.go") || !strings.Contains(stack, "TestStackTraceHandling.func") {
			t.Errorf("Custom stack trace does not appear to originate from the test's anonymous helper. Got: %s", stack)
		}
	})

	t.Run("WrappedCustomErrorStackTrace", func(t *testing.T) {
		const logMsg = "test wrapped custom stack trace"
		const innerErrMsg = "inner error with custom stack"
		var output string

		// specificTestFuncWrapped creates a wrapped error. The inner error provides the stack.
		var specificTestFuncWrapped = func() error {
			innerCustomErr := newCustomErrorWithStackTrace(innerErrMsg, 0)
			return fmt.Errorf("wrapper for testing: %w", innerCustomErr)
		}
		wrappedCustomErr := specificTestFuncWrapped()

		output = captureStdOutput(t, func() {
			logger, err := slogcp.New(
				slogcp.WithRedirectToStdout(),
				slogcp.WithStackTraceEnabled(true),
				slogcp.WithStackTraceLevel(slog.LevelError),
			)
			if err != nil {
				t.Fatalf("New() failed: %v", err)
			}
			defer logger.Close()
			logger.ErrorContext(context.Background(), logMsg, slog.Any("error", wrappedCustomErr))
		})

		entry := findLogEntry(t, output, logMsg)
		stack, ok := entry["stack_trace"].(string)
		if !ok {
			t.Fatalf("stack_trace field: got none or non-string for wrapped error. Entry: %+v", entry)
		}
		// The stack trace should come from the *inner* custom error, which was created
		// inside 'specificTestFuncWrapped' (an anonymous function).
		if !strings.Contains(stack, "stack_unit_test.go") || !strings.Contains(stack, "TestStackTraceHandling.func") {
			t.Errorf("Wrapped custom stack trace does not appear to originate from the inner error's creation site. Got: %s", stack)
		}
	})

	t.Run("StackTraceDisabled", func(t *testing.T) {
		const logMsg = "test stack trace disabled"
		output := captureStdOutput(t, func() {
			logger, err := slogcp.New(
				slogcp.WithRedirectToStdout(),
				slogcp.WithStackTraceEnabled(false), // Stack traces explicitly disabled.
			)
			if err != nil {
				t.Fatalf("New() failed: %v", err)
			}
			defer logger.Close()
			logger.ErrorContext(context.Background(), logMsg, slog.Any("error", errors.New("some error")))
		})

		entry := findLogEntry(t, output, logMsg)
		if _, ok := entry["stack_trace"]; ok {
			t.Errorf("stack_trace field: got field, want none as traces disabled. Entry: %+v", entry)
		}
	})

	t.Run("LogLevelBelowStackTraceLevel", func(t *testing.T) {
		const logMsg = "test log level below stack"
		output := captureStdOutput(t, func() {
			logger, err := slogcp.New(
				slogcp.WithRedirectToStdout(),
				slogcp.WithStackTraceEnabled(true),
				slogcp.WithStackTraceLevel(slog.LevelError), // Stacks for ERROR and above.
			)
			if err != nil {
				t.Fatalf("New() failed: %v", err)
			}
			defer logger.Close()
			// Log at WARN level, which is below the configured stack trace level.
			logger.WarnContext(context.Background(), logMsg, slog.Any("error", errors.New("warning error")))
		})

		entry := findLogEntry(t, output, logMsg)
		if _, ok := entry["stack_trace"]; ok {
			t.Errorf("stack_trace field: got field, want none as log level too low. Entry: %+v", entry)
		}
	})

	t.Run("NilErrorWithStackTraceEnabled", func(t *testing.T) {
		const logMsg = "test nil error"
		output := captureStdOutput(t, func() {
			logger, err := slogcp.New(
				slogcp.WithRedirectToStdout(),
				slogcp.WithStackTraceEnabled(true),
				slogcp.WithStackTraceLevel(slog.LevelError),
			)
			if err != nil {
				t.Fatalf("New() failed: %v", err)
			}
			defer logger.Close()
			logger.ErrorContext(context.Background(), logMsg, slog.Any("error", nil))
		})

		entry := findLogEntry(t, output, logMsg)
		if _, ok := entry["stack_trace"]; ok {
			t.Errorf("stack_trace field: got field for nil error, want none. Entry: %+v", entry)
		}
		// The "error" attribute itself should be absent or explicitly null when slog.Any("error", nil) is logged.
		errField, errExists := entry["error"]
		if errExists && errField != nil {
			t.Errorf("error field: got %v, want nil or absent for nil error. Entry: %+v", errField, entry)
		}
	})

	t.Run("LogMessageWithoutErrorObject", func(t *testing.T) {
		const logMsg = "error message no object"
		output := captureStdOutput(t, func() {
			logger, err := slogcp.New(
				slogcp.WithRedirectToStdout(),
				slogcp.WithStackTraceEnabled(true),
				slogcp.WithStackTraceLevel(slog.LevelError),
			)
			if err != nil {
				t.Fatalf("New() failed: %v", err)
			}
			defer logger.Close()
			logger.Error(logMsg) // Logging a string message at Error level, not an error object.
		})

		entry := findLogEntry(t, output, logMsg)
		// No error object means no 'firstErr' in handler, so no stack trace logic is triggered.
		if _, ok := entry["stack_trace"]; ok {
			t.Errorf("stack_trace field: got field, want none as no error object logged. Entry: %+v", entry)
		}
	})

	t.Run("StackTraceEnabledViaEnvVar", func(t *testing.T) {
		t.Setenv("LOG_STACK_TRACE_ENABLED", "true")
		// LOG_STACK_TRACE_LEVEL defaults to slog.LevelError if not set.
		// LOG_LEVEL defaults to slog.LevelInfo if not set.
		defer t.Setenv("LOG_STACK_TRACE_ENABLED", "") // Cleanup.

		const logMsg = "test stack trace from env"
		output := captureStdOutput(t, func() {
			logger, err := slogcp.New(slogcp.WithRedirectToStdout()) // Relies on env vars.
			if err != nil {
				t.Fatalf("New() failed: %v", err)
			}
			defer logger.Close()
			logger.ErrorContext(context.Background(), logMsg, slog.Any("error", errors.New("env var error")))
		})

		entry := findLogEntry(t, output, logMsg)
		stack, ok := entry["stack_trace"].(string)
		if !ok {
			t.Errorf("stack_trace field: got none, want string from env var. Entry: %+v", entry)
		}
		if stack == "" {
			t.Errorf("stack_trace field: got empty, want non-empty from env var. Entry: %+v", entry)
		}
	})

	t.Run("StackTraceLevelViaEnvVar", func(t *testing.T) {
		t.Setenv("LOG_STACK_TRACE_ENABLED", "true")
		t.Setenv("LOG_STACK_TRACE_LEVEL", "WARN") // Stacks for WARN and above.
		defer func() {
			t.Setenv("LOG_STACK_TRACE_ENABLED", "")
			t.Setenv("LOG_STACK_TRACE_LEVEL", "")
		}() // Cleanup.

		const logMsgWarn = "test stack trace level env warn"
		const logMsgInfo = "test stack trace level env info" // Below WARN.
		output := captureStdOutput(t, func() {
			logger, err := slogcp.New(slogcp.WithRedirectToStdout()) // Relies on env vars.
			if err != nil {
				t.Fatalf("New() failed: %v", err)
			}
			defer logger.Close()
			logger.WarnContext(context.Background(), logMsgWarn, slog.Any("error", errors.New("env var warning error")))
			logger.InfoContext(context.Background(), logMsgInfo, slog.Any("error", errors.New("env var info error")))
		})

		entryWarn := findLogEntry(t, output, logMsgWarn)
		if _, ok := entryWarn["stack_trace"]; !ok {
			t.Errorf("stack_trace field (WARN): got none, want string with env var. Entry: %+v", entryWarn)
		}

		entryInfo := findLogEntry(t, output, logMsgInfo)
		if _, ok := entryInfo["stack_trace"]; ok {
			t.Errorf("stack_trace field (INFO): got field, want none with env var. Entry: %+v", entryInfo)
		}
	})
}
