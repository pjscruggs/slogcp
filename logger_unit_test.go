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
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pjscruggs/slogcp"
)

// captureOutput redirects os.Stdout or os.Stderr, runs the provided function,
// captures the output, and restores the original stream.
// Tests using this cannot run in parallel.
func captureOutput(t *testing.T, stream **os.File, action func()) string {
	t.Helper()
	original := *stream
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("captureOutput: Failed to create pipe: %v", err)
	}
	*stream = w

	t.Cleanup(func() {
		*stream = original
		if err := r.Close(); err != nil {
			t.Logf("Error closing pipe reader: %v", err)
		}
	})

	outC := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, copyErr := io.Copy(&buf, r)
		// Ignore pipe closed errors, which can happen normally during cleanup.
		if copyErr != nil && !errors.Is(copyErr, os.ErrClosed) && !errors.Is(copyErr, io.ErrClosedPipe) {
			t.Logf("captureOutput: Error copying stream: %v", copyErr)
		}
		outC <- buf.String()
	}()

	action()
	// Close the writer end of the pipe to signal EOF to the reading goroutine.
	// Ignore error as the reader might have already closed the pipe.
	_ = w.Close()

	return <-outC
}

// TestStructuredJsonLogging verifies that the logger properly formats structured 
// logs with the expected content and respects log level settings. It ensures
// attributes are correctly grouped, levels control message filtering, and
// that source location is included when enabled.
func TestStructuredJsonLogging(t *testing.T) {
	// Capture stdout for verification
	output := captureOutput(t, &os.Stdout, func() {
		// Setup a logger with stdout redirection and DEBUG level
		logger, err := slogcp.New(
			slogcp.WithRedirectToStdout(),
			slogcp.WithLevel(slog.LevelDebug),
			slogcp.WithSourceLocationEnabled(true),
		)
		if err != nil {
			t.Fatalf("Failed to create logger: %v", err)
		}
		defer logger.Close()

		// Create a child logger with attributes and group
		childLogger := logger.With("component", "user-service").WithGroup("request")

		// Log messages at different levels
		logger.Debug("This is a debug message")
		logger.Info("This is an info message")
		childLogger.Info("Processing request", "user_id", "123", "action", "login")
		logger.Warn("This is a warning", "attempt", 3)
		logger.Error("This is an error", "code", 500)

		// Add a dynamic log level test
		logger.SetLevel(slog.LevelWarn)
		logger.Info("This should be suppressed")
		logger.Warn("This warning should appear")
	})

	// Check for expected log presence
	if !strings.Contains(output, "This is a debug message") {
		t.Error("Debug message missing when level set to Debug")
	}

	// After changing level to Warn, Info messages should be suppressed
	lines := strings.Split(output, "\n")
	foundSuppressedInfo := false
	for _, line := range lines {
		if strings.Contains(line, "This should be suppressed") {
			foundSuppressedInfo = true
			break
		}
	}
	if foundSuppressedInfo {
		t.Error("Info message appeared despite level being set to Warn")
	}

	// Verify warning after level change appears
	if !strings.Contains(output, "This warning should appear") {
		t.Error("Warning message missing after setting level to Warn")
	}

	// Verify structure by parsing the structured log with request group
	var processRequestFound bool
	for _, line := range lines {
		if strings.Contains(line, "Processing request") {
			processRequestFound = true
			var entry map[string]interface{}
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Errorf("Failed to parse JSON log entry: %v", err)
				continue
			}

			// Check for expected fields in the structured log
			if entry["severity"] != "INFO" {
				t.Errorf("Expected severity INFO, got %v", entry["severity"])
			}

			// Verify time field exists and parses as a valid timestamp
			timeStr, ok := entry["time"].(string)
			if !ok || timeStr == "" {
				t.Error("Missing or invalid time field")
			} else {
				_, err := time.Parse(time.RFC3339Nano, timeStr)
				if err != nil {
					t.Errorf("Invalid timestamp format: %v", err)
				}
			}

			// Verify message field
			if entry["message"] != "Processing request" {
				t.Errorf("Expected message 'Processing request', got %v", entry["message"])
			}

			// Verify attributes and grouping
			request, ok := entry["request"].(map[string]interface{})
			if !ok {
				t.Error("Missing or invalid 'request' group")
			} else {
				if request["user_id"] != "123" || request["action"] != "login" {
					t.Errorf("Incorrect attributes in request group: %v", request)
				}
			}

			// Verify component attribute
			if comp, ok := entry["component"].(string); !ok || comp != "user-service" {
				t.Errorf("Component attribute missing or incorrect: %v", entry["component"])
			}

			// Verify source location if enabled
			srcLoc, ok := entry["logging.googleapis.com/sourceLocation"].(map[string]interface{})
			if !ok {
				t.Error("Missing source location despite being enabled")
			} else if srcLoc["file"] == "" || srcLoc["line"] == nil {
				t.Error("Incomplete source location info")
			}

			break
		}
	}

	if !processRequestFound {
		t.Error("Structured log entry with 'Processing request' not found")
	}
}

// TestSourceLocation verifies that source code location information is properly
// included in log entries when enabled and correctly omitted when disabled.
// This tests the WithSourceLocationEnabled option's effect on log output.
func TestSourceLocation(t *testing.T) {
	t.Run("SourceEnabled", func(t *testing.T) {
		output := captureOutput(t, &os.Stdout, func() {
			logger, err := slogcp.New(
				slogcp.WithRedirectToStdout(),
				slogcp.WithSourceLocationEnabled(true),
			)
			if err != nil {
				t.Fatalf("Failed to create logger: %v", err)
			}
			defer logger.Close()

			logger.Info("source test enabled")
		})

		// Verify the output contains the message
		if !strings.Contains(output, "source test enabled") {
			t.Errorf("Log message not found in output")
		}

		// Parse and check for source location
		var entry map[string]interface{}
		scanner := bufio.NewScanner(strings.NewReader(output))
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "source test enabled") {
				err := json.Unmarshal([]byte(line), &entry)
				if err != nil {
					t.Fatalf("Failed to unmarshal JSON: %v", err)
				}
				break
			}
		}

		// Verify source location is present
		srcLoc, ok := entry["logging.googleapis.com/sourceLocation"].(map[string]interface{})
		if !ok {
			t.Error("Source location not found in log entry")
		} else if srcLoc["file"] == "" || srcLoc["line"] == nil {
			t.Error("Incomplete source location info")
		}
	})

	t.Run("SourceDisabled", func(t *testing.T) {
		output := captureOutput(t, &os.Stdout, func() {
			logger, err := slogcp.New(
				slogcp.WithRedirectToStdout(),
				slogcp.WithSourceLocationEnabled(false),
			)
			if err != nil {
				t.Fatalf("Failed to create logger: %v", err)
			}
			defer logger.Close()

			logger.Info("source test disabled")
		})

		// Verify the output contains the message
		if !strings.Contains(output, "source test disabled") {
			t.Errorf("Log message not found in output")
		}

		// Parse and check that source location is not present
		var entry map[string]interface{}
		scanner := bufio.NewScanner(strings.NewReader(output))
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "source test disabled") {
				err := json.Unmarshal([]byte(line), &entry)
				if err != nil {
					t.Fatalf("Failed to unmarshal JSON: %v", err)
				}
				break
			}
		}

		// Verify source location is not present
		_, ok := entry["logging.googleapis.com/sourceLocation"]
		if ok {
			t.Error("Source location found in log entry despite being disabled")
		}
	})
}

// TestDynamicLogLevels validates that log levels can be set and changed
// during runtime and that messages are filtered appropriately.
func TestDynamicLogLevels(t *testing.T) {
	// Create a test that verifies all the extended log levels provided by slogcp
	levels := []struct {
		name  string
		level slog.Level
	}{
		{"Default", slogcp.LevelDefault.Level()},
		{"Debug", slog.LevelDebug},
		{"Info", slog.LevelInfo},
		{"Notice", slogcp.LevelNotice.Level()},
		{"Warn", slog.LevelWarn},
		{"Error", slog.LevelError},
		{"Critical", slogcp.LevelCritical.Level()},
		{"Alert", slogcp.LevelAlert.Level()},
		{"Emergency", slogcp.LevelEmergency.Level()},
	}

	for i, lvl := range levels {
		t.Run(lvl.name, func(t *testing.T) {
			output := captureOutput(t, &os.Stdout, func() {
				logger, err := slogcp.New(
					slogcp.WithRedirectToStdout(),
					slogcp.WithLevel(lvl.level),
				)
				if err != nil {
					t.Fatalf("Failed to create logger: %v", err)
				}
				defer logger.Close()

				// Verify GetLevel returns the expected level
				if got := logger.GetLevel(); got != lvl.level {
					t.Errorf("GetLevel() = %v, want %v", got, lvl.level)
				}

				// Log at all levels and check which ones appear
				ctx := context.Background()
				logger.DefaultContext(ctx, "default test")
				logger.Debug("debug test")
				logger.Info("info test")
				logger.NoticeContext(ctx, "notice test")
				logger.Warn("warn test")
				logger.Error("error test")
				logger.CriticalContext(ctx, "critical test")
				logger.AlertContext(ctx, "alert test")
				logger.EmergencyContext(ctx, "emergency test")
			})

			// Parse and check levels
			var logEntries []map[string]interface{}
			scanner := bufio.NewScanner(strings.NewReader(output))
			for scanner.Scan() {
				line := scanner.Text()
				if line == "" {
					continue
				}

				var entry map[string]interface{}
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Errorf("Failed to parse JSON log entry: %v", err)
					continue
				}
				logEntries = append(logEntries, entry)
			}

			// Verify that only messages at or above the set level appear
			for j, checkLvl := range levels {
				levelShouldAppear := j >= i // current level index >= test level index
				levelName := strings.ToLower(checkLvl.name)
				
				found := false
				for _, entry := range logEntries {
					msg, _ := entry["message"].(string)
					if strings.Contains(msg, levelName+" test") {
						found = true
						break
					}
				}

				if levelShouldAppear && !found {
					t.Errorf("Message at level %s should appear but didn't", checkLvl.name)
				} else if !levelShouldAppear && found {
					t.Errorf("Message at level %s should be filtered out but appeared", checkLvl.name)
				}
			}
		})
	}
}

// TestLogTargets validates that the logger can be configured to send logs to
// different destinations (stdout, stderr) and that logs appear in the expected place.
func TestLogTargets(t *testing.T) {
	testCases := []struct {
		name          string
		createOptions []slogcp.Option
		targetStream  **os.File
	}{
		{
			name:          "StdoutTarget",
			createOptions: []slogcp.Option{slogcp.WithLogTarget(slogcp.LogTargetStdout)},
			targetStream:  &os.Stdout,
		},
		{
			name:          "StdoutRedirect",
			createOptions: []slogcp.Option{slogcp.WithRedirectToStdout()},
			targetStream:  &os.Stdout,
		},
		{
			name:          "StderrTarget",
			createOptions: []slogcp.Option{slogcp.WithLogTarget(slogcp.LogTargetStderr)},
			targetStream:  &os.Stderr,
		},
		{
			name:          "StderrRedirect",
			createOptions: []slogcp.Option{slogcp.WithRedirectToStderr()},
			targetStream:  &os.Stderr,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			output := captureOutput(t, tc.targetStream, func() {
				logger, err := slogcp.New(tc.createOptions...)
				if err != nil {
					t.Fatalf("Failed to create logger: %v", err)
				}
				defer logger.Close()

				// Log a test message
				logger.Info("target test message", "target", tc.name)
			})

			// Verify that the log message appears in the output
			if !strings.Contains(output, "target test message") {
				t.Errorf("Log message not found in captured output")
			}

			// Parse JSON to verify structure
			var found bool
			scanner := bufio.NewScanner(strings.NewReader(output))
			for scanner.Scan() {
				line := scanner.Text()
				if strings.Contains(line, "target test message") {
					var entry map[string]interface{}
					if err := json.Unmarshal([]byte(line), &entry); err != nil {
						t.Errorf("Failed to parse JSON: %v", err)
						continue
					}

					found = true
					msg, _ := entry["message"].(string)
					if msg != "target test message" {
						t.Errorf("Message = %q, want 'target test message'", msg)
					}

					targetVal, _ := entry["target"].(string)
					if targetVal != tc.name {
						t.Errorf("Target attribute = %q, want %q", targetVal, tc.name)
					}

					break
				}
			}

			if !found {
				t.Error("No valid JSON log entry found in output")
			}
		})
	}
}

// TestFallbackMode verifies that fallback to local logging happens correctly
// when GCP logging is unavailable but no explicit target is specified.
func TestFallbackMode(t *testing.T) {
	// In a normal environment without GCP credentials, New() without options
	// should fall back to stdout logging

	stderrOutput := captureOutput(t, &os.Stderr, func() {
		stdoutOutput := captureOutput(t, &os.Stdout, func() {
			// Clear any environment variables that might affect the test
			t.Setenv("GOOGLE_CLOUD_PROJECT", "")
			t.Setenv("SLOGCP_GCP_PARENT", "")
			t.Setenv("SLOGCP_PROJECT_ID", "")
			t.Setenv("SLOGCP_LOG_TARGET", "")
			
			// Create logger without options - should attempt GCP and fall back
			logger, err := slogcp.New()
			if err != nil {
				t.Fatalf("New() failed with error: %v", err)
			}
			defer logger.Close()

			// Verify we're in fallback mode
			if !logger.IsInFallbackMode() {
				t.Error("Expected IsInFallbackMode() to return true")
			}

			// Log a message - should appear on stdout
			logger.Info("fallback test message")
		})

		// Verify that the message appears on stdout
		if !strings.Contains(stdoutOutput, "fallback test message") {
			t.Errorf("Fallback log message not found in stdout output:\n%s", stdoutOutput)
		}
	})

	// Verify stderr contains appropriate warning
	if !strings.Contains(stderrOutput, "Failed to initialize GCP") {
		t.Errorf("Expected fallback warning in stderr, got:\n%s", stderrOutput)
	}
}

// TestLogLevelFromEnvironment verifies that log level can be set via environment variable
func TestLogLevelFromEnvironment(t *testing.T) {
	t.Setenv("LOG_LEVEL", "ERROR")

	output := captureOutput(t, &os.Stdout, func() {
		logger, err := slogcp.New(slogcp.WithRedirectToStdout())
		if err != nil {
			t.Fatalf("Failed to create logger: %v", err)
		}
		defer logger.Close()

		// Log at different levels
		logger.Debug("debug env test")
		logger.Info("info env test")
		logger.Warn("warn env test")
		logger.Error("error env test")
	})

	// Verify only ERROR message appears
	if strings.Contains(output, "debug env test") {
		t.Error("Debug message should not appear")
	}
	if strings.Contains(output, "info env test") {
		t.Error("Info message should not appear")
	}
	if strings.Contains(output, "warn env test") {
		t.Error("Warn message should not appear")
	}
	if !strings.Contains(output, "error env test") {
		t.Error("Error message should appear")
	}
}

// TestReplaceAttrFunc verifies that the WithReplaceAttr option correctly
// transforms log attributes.
func TestReplaceAttrFunc(t *testing.T) {
	output := captureOutput(t, &os.Stdout, func() {
		// Create a logger with custom attribute replacer
		logger, err := slogcp.New(
			slogcp.WithRedirectToStdout(),
			slogcp.WithReplaceAttr(func(groups []string, a slog.Attr) slog.Attr {
				// Replace "secret" with "[REDACTED]"
				if a.Key == "secret" {
					return slog.String("secret", "[REDACTED]")
				}
				// Add prefix to keys in the "request" group
				if len(groups) == 1 && groups[0] == "request" {
					return slog.String("req_"+a.Key, a.Value.String())
				}
				return a
			}),
		)
		if err != nil {
			t.Fatalf("Failed to create logger: %v", err)
		}
		defer logger.Close()

		// Log with attributes that should be transformed
		logger.Info("replace test", "secret", "password123")
		logger.WithGroup("request").Info("request info", "path", "/api/users", "method", "GET")
	})

	// Check for redacted secret
	if strings.Contains(output, "password123") {
		t.Error("Secret value was not redacted")
	}
	if !strings.Contains(output, "[REDACTED]") {
		t.Error("Redacted placeholder not found")
	}

	// Check for prefixed request attributes
	if strings.Contains(output, `"path":`) {
		t.Error("Original 'path' key was not replaced")
	}
	if !strings.Contains(output, `"req_path":`) {
		t.Error("Transformed 'req_path' key not found")
	}
}

// middlewareTestHandler is a handler wrapper for testing middleware functionality.
// It wraps an existing handler and allows injecting additional behavior.
type middlewareTestHandler struct {
	next    slog.Handler
	addAttr func(*slog.Record)
}

func (h *middlewareTestHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *middlewareTestHandler) Handle(ctx context.Context, r slog.Record) error {
	// Apply the middleware transformation
	if h.addAttr != nil {
		h.addAttr(&r)
	}
	// Delegate to the next handler
	return h.next.Handle(ctx, r)
}

func (h *middlewareTestHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &middlewareTestHandler{
		next:    h.next.WithAttrs(attrs),
		addAttr: h.addAttr,
	}
}

func (h *middlewareTestHandler) WithGroup(name string) slog.Handler {
	return &middlewareTestHandler{
		next:    h.next.WithGroup(name),
		addAttr: h.addAttr,
	}
}

// TestMiddleware verifies that middleware functions transform logs correctly
func TestMiddleware(t *testing.T) {
	// attributeMiddleware returns a slogcp.Middleware that adds key/value to logs
	attributeMiddleware := func(key, value string) slogcp.Middleware {
		return func(h slog.Handler) slog.Handler {
			return &middlewareTestHandler{
				next: h,
				addAttr: func(r *slog.Record) {
					r.AddAttrs(slog.String(key, value))
				},
			}
		}
	}

	output := captureOutput(t, &os.Stdout, func() {
		// Create logger with multiple middleware
		logger, err := slogcp.New(
			slogcp.WithRedirectToStdout(),
			slogcp.WithMiddleware(attributeMiddleware("mw1", "value1")),
			slogcp.WithMiddleware(nil), // Should be safely ignored
			slogcp.WithMiddleware(attributeMiddleware("mw2", "value2")),
		)
		if err != nil {
			t.Fatalf("Failed to create logger: %v", err)
		}
		defer logger.Close()

		logger.Info("middleware test")
	})

	// Parse and verify middleware attributes
	var entry map[string]interface{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "middleware test") {
			err := json.Unmarshal([]byte(line), &entry)
			if err != nil {
				t.Fatalf("Failed to unmarshal JSON: %v", err)
			}
			break
		}
	}

	// Check that both middleware attributes were added
	if entry["mw1"] != "value1" {
		t.Errorf("First middleware attribute missing or incorrect: %v", entry)
	}
	if entry["mw2"] != "value2" {
		t.Errorf("Second middleware attribute missing or incorrect: %v", entry)
	}
}
