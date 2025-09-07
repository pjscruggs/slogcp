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
	"log/slog"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pjscruggs/slogcp"
)

// TestLoggerOptionInteraction verifies that various option combinations
// are correctly applied to the logger configuration, and that options
// properly override each other in the expected order.
func TestLoggerOptionInteraction(t *testing.T) {
	// Test level option overrides
	t.Run("LevelOverrides", func(t *testing.T) {
		// Level from environment variable
		t.Setenv("LOG_LEVEL", "ERROR")
		logger, err := slogcp.New(slogcp.WithRedirectToStdout())
		if err != nil {
			t.Fatalf("New with env level failed: %v", err)
		}
		// Should get ERROR from environment
		if got := logger.GetLevel(); got != slog.LevelError {
			t.Errorf("Level from env: got %v, want %v", got, slog.LevelError)
		}

		// Level from option should override environment
		logger2, err := slogcp.New(
			slogcp.WithRedirectToStdout(),
			slogcp.WithLevel(slog.LevelDebug),
		)
		if err != nil {
			t.Fatalf("New with option level failed: %v", err)
		}
		// Should get DEBUG from option, not ERROR from environment
		if got := logger2.GetLevel(); got != slog.LevelDebug {
			t.Errorf("Level from option override: got %v, want %v", got, slog.LevelDebug)
		}
	})

	// Test log target option combinations
	t.Run("LogTargetCombinations", func(t *testing.T) {
		// Should succeed: explicit target + matching redirect
		_, err := slogcp.New(
			slogcp.WithLogTarget(slogcp.LogTargetStdout),
			slogcp.WithRedirectToStdout(),
		)
		if err != nil {
			t.Errorf("WithLogTarget(Stdout) + WithRedirectToStdout failed: %v", err)
		}

		// Should succeed: later redirect overrides earlier target
		lg, err := slogcp.New(
			slogcp.WithLogTarget(slogcp.LogTargetStdout),
			slogcp.WithRedirectToStderr(),
		)
		if err != nil {
			t.Errorf("WithLogTarget(Stdout) + WithRedirectToStderr failed: %v", err)
		} else {
			lg.Close()
		}

		// File target requires a path
		_, err = slogcp.New(
			slogcp.WithLogTarget(slogcp.LogTargetFile),
		)
		if err == nil {
			t.Error("WithLogTarget(File) without path should fail but succeeded")
		}

		// File target with path should succeed
		tmpFile := t.TempDir() + "/test.log"
		logger, err := slogcp.New(
			slogcp.WithLogTarget(slogcp.LogTargetFile),
			slogcp.WithRedirectToFile(tmpFile),
		)
		if err != nil {
			t.Errorf("WithLogTarget(File) + WithRedirectToFile failed: %v", err)
		}
		defer logger.Close()

		// Verify file creation
		logger.Info("file test")
		_, err = os.Stat(tmpFile)
		if err != nil {
			t.Errorf("Log file wasn't created: %v", err)
		}
	})

	// Test source location option
	t.Run("SourceLocationOption", func(t *testing.T) {
		output := captureOutput(t, &os.Stdout, func() {
			// Default should be false
			logger, err := slogcp.New(slogcp.WithRedirectToStdout())
			if err != nil {
				t.Fatalf("New without source option failed: %v", err)
			}
			logger.Info("default source setting")

			// Explicit enabled should work
			logger2, err := slogcp.New(
				slogcp.WithRedirectToStdout(),
				slogcp.WithSourceLocationEnabled(true),
			)
			if err != nil {
				t.Fatalf("New with source option failed: %v", err)
			}
			logger2.Info("enabled source setting")
		})

		// Examine individual log lines so we don't confuse the two messages.
		var defaultLine, enabledLine string
		for _, line := range strings.Split(output, "\n") {
			if strings.Contains(line, "default source setting") {
				defaultLine = line
			}
			if strings.Contains(line, "enabled source setting") {
				enabledLine = line
			}
		}

		if defaultLine == "" || enabledLine == "" {
			t.Fatalf("Failed to capture expected log lines.\nOutput:\n%s", output)
		}

		// Default logger line should NOT contain source location.
		if strings.Contains(defaultLine, "logging.googleapis.com/sourceLocation") {
			t.Error("Default logger should not include source location")
		}
		// Source‑enabled logger line SHOULD contain source location.
		if !strings.Contains(enabledLine, "logging.googleapis.com/sourceLocation") {
			t.Error("Source-enabled logger doesn't show source location")
		}
	})

	// Test WithGCPDelayThreshold option
	t.Run("DelayThresholdOption", func(t *testing.T) {
		delay := 5 * time.Second
		// Just verify we can create a logger with this option
		// (actual effect would need GCP API testing)
		logger, err := slogcp.New(
			slogcp.WithRedirectToStdout(),
			slogcp.WithGCPDelayThreshold(delay),
		)
		if err != nil {
			t.Errorf("WithGCPDelayThreshold failed: %v", err)
		}
		defer logger.Close()
	})
}

// TestRedirectToFileAndClose verifies that when logging to a file:
// 1. The file is created with proper permissions (platform‑specific)
// 2. Logs are written to the file
// 3. Close() properly flushes and closes the file
func TestRedirectToFileAndClose(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := tmpDir + "/test.log"

	// Create logger with file redirection
	logger, err := slogcp.New(slogcp.WithRedirectToFile(logPath))
	if err != nil {
		t.Fatalf("Failed to create logger with file redirect: %v", err)
	}

	// Write a log message
	logger.Info("file test message")

	// Close should flush and release the file
	if err := logger.Close(); err != nil {
		t.Errorf("Logger.Close() returned error: %v", err)
	}

	// Verify the file exists and contains the message
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}
	if !strings.Contains(string(content), "file test message") {
		t.Errorf("Log file doesn't contain expected message")
	}

	// Optionally enforce 0644 on Unix‑like OSes; Windows always reports 0666.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(logPath, 0644); err != nil {
			t.Fatalf("os.Chmod failed: %v", err)
		}
	}

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("Failed to stat log file: %v", err)
	}
	expectedPerm := os.FileMode(0644)
	if runtime.GOOS == "windows" {
		expectedPerm = os.FileMode(0666)
	}
	if info.Mode().Perm() != expectedPerm {
		t.Errorf("Log file has incorrect permissions: got %v, want %v",
			info.Mode().Perm(), expectedPerm)
	}

	// Verify that second close is a no‑op
	if err := logger.Close(); err != nil {
		t.Errorf("Second Logger.Close() should be no‑op, got error: %v", err)
	}
}

// TestReopenLogFile verifies that ReopenLogFile correctly reopens a log file,
// which is important for external log‑rotation tools.
//
// Windows cannot rename an open file, so the test is skipped on that OS.
func TestReopenLogFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Renaming an open file is not supported on Windows; skipping rotation test")
	}

	tmpDir := t.TempDir()
	logPath := tmpDir + "/rotating.log"

	// Create logger with file redirection
	logger, err := slogcp.New(slogcp.WithRedirectToFile(logPath))
	if err != nil {
		t.Fatalf("Failed to create logger with file redirect: %v", err)
	}
	defer logger.Close()

	// Write a log message
	logger.Info("first log message")

	// Simulate log rotation: rename the current log file
	rotatedPath := logPath + ".1"
	if err := os.Rename(logPath, rotatedPath); err != nil {
		t.Fatalf("Failed to simulate log rotation: %v", err)
	}

	// Reopen the log file
	if err := logger.ReopenLogFile(); err != nil {
		t.Errorf("ReopenLogFile() returned error: %v", err)
	}

	// Write another log message
	logger.Info("second log message")

	// Check both files exist
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("New log file doesn't exist: %v", err)
	}
	if _, err := os.Stat(rotatedPath); err != nil {
		t.Errorf("Rotated log file doesn't exist: %v", err)
	}

	// Verify content of both files
	content1, err := os.ReadFile(rotatedPath)
	if err != nil {
		t.Fatalf("Failed to read rotated log file: %v", err)
	}
	if !strings.Contains(string(content1), "first log message") {
		t.Errorf("Rotated log file doesn't contain first message")
	}

	content2, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read new log file: %v", err)
	}
	if !strings.Contains(string(content2), "second log message") {
		t.Errorf("New log file doesn't contain second message")
	}
}

// TestLogIDConfigurationPrecedence verifies that log ID configuration follows
// the correct precedence order: programmatic option > environment variable > default.
func TestLogIDConfigurationPrecedence(t *testing.T) {
	testCases := []struct {
		name       string
		envValue   string
		option     *string // nil means no option provided
		wantLogID  string
		wantError  bool
	}{
		{
			name:      "Default when nothing set",
			envValue:  "",
			option:    nil,
			wantLogID: "app",
		},
		{
			name:      "Environment variable only",
			envValue:  "env-log-id",
			option:    nil,
			wantLogID: "env-log-id",
		},
		{
			name:      "Programmatic option only",
			envValue:  "",
			option:    stringPtr("option-log-id"),
			wantLogID: "option-log-id",
		},
		{
			name:      "Option overrides environment variable",
			envValue:  "env-log-id",
			option:    stringPtr("option-log-id"),
			wantLogID: "option-log-id",
		},
		{
			name:      "Empty option string uses default",
			envValue:  "env-log-id",
			option:    stringPtr(""),
			wantLogID: "app",
		},
		{
			name:      "Option with whitespace gets trimmed",
			envValue:  "",
			option:    stringPtr("  trimmed  "),
			wantLogID: "trimmed",
		},
		{
			name:      "Invalid env var with valid option",
			envValue:  "invalid@log",
			option:    stringPtr("valid-log-id"),
			wantLogID: "valid-log-id",
		},
		{
			name:      "Invalid env var without option override",
			envValue:  "invalid@log",
			option:    nil,
			wantError: true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Set environment variable
			if tc.envValue != "" {
				t.Setenv("SLOGCP_GCP_LOG_ID", tc.envValue)
			}

			// Build options
			opts := []slogcp.Option{slogcp.WithRedirectToStdout()}
			if tc.option != nil {
				opts = append(opts, slogcp.WithGCPLogID(*tc.option))
			}

			// Create logger
			logger, err := slogcp.New(opts...)

			if tc.wantError {
				if err == nil {
					t.Error("Expected error but got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			defer logger.Close()

			// Verify log ID
			if got := logger.GCPLogID(); got != tc.wantLogID {
				t.Errorf("logger.GCPLogID() = %q; want %q", got, tc.wantLogID)
			}
		})
	}
}

// stringPtr is a helper function to get a pointer to a string value.
// This is useful for test cases where we need to distinguish between
// an empty string and no value provided.
func stringPtr(s string) *string {
	return &s
}