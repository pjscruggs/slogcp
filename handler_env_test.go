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

package slogcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// newDiscardLogger returns a test logger that discards all output.
func newDiscardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// clearHandlerEnv resets the environment variables that influence handler configuration.
func clearHandlerEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envLogLevel, "")
	t.Setenv(envLogSource, "")
	t.Setenv(envLogTime, "")
	t.Setenv(envLogStackEnabled, "")
	t.Setenv(envLogStackLevel, "")
	t.Setenv(envSeverityAliases, "")
	t.Setenv(envTraceProjectID, "")
	t.Setenv(envProjectID, "")
	t.Setenv(envSlogcpGCP, "")
	t.Setenv(envGoogleProject, "")
	t.Setenv(envTarget, "")
	resetRuntimeInfoCache()
	resetHandlerConfigCache()
}

// TestLoadConfigFromEnvLevelOverride verifies environment overrides adjust the minimum level.
func TestLoadConfigFromEnvLevelOverride(t *testing.T) {
	clearHandlerEnv(t)
	t.Setenv(envLogLevel, "warning")

	cfg, err := loadConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
	}
	if cfg.Level != slog.LevelWarn {
		t.Fatalf("cfg.Level = %v, want %v", cfg.Level, slog.LevelWarn)
	}
}

// TestLoadConfigFromEnvBoolFlags ensures boolean environment variables are interpreted correctly.
func TestLoadConfigFromEnvBoolFlags(t *testing.T) {
	tests := []struct {
		name   string
		envVar string
		value  string
		assert func(t *testing.T, cfg handlerConfig)
	}{
		{
			name:   "source_location",
			envVar: envLogSource,
			value:  "true",
			assert: func(t *testing.T, cfg handlerConfig) {
				if !cfg.AddSource {
					t.Fatalf("cfg.AddSource = false, want true")
				}
			},
		},
		{
			name:   "time_field",
			envVar: envLogTime,
			value:  "1",
			assert: func(t *testing.T, cfg handlerConfig) {
				if !cfg.EmitTimeField {
					t.Fatalf("cfg.EmitTimeField = false, want true")
				}
			},
		},
		{
			name:   "stack_trace_enabled",
			envVar: envLogStackEnabled,
			value:  "true",
			assert: func(t *testing.T, cfg handlerConfig) {
				if !cfg.StackTraceEnabled {
					t.Fatalf("cfg.StackTraceEnabled = false, want true")
				}
			},
		},
		{
			name:   "severity_aliases",
			envVar: envSeverityAliases,
			value:  "false",
			assert: func(t *testing.T, cfg handlerConfig) {
				if cfg.UseShortSeverityNames {
					t.Fatalf("cfg.UseShortSeverityNames = true, want false")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearHandlerEnv(t)
			t.Setenv(tt.envVar, tt.value)

			cfg, err := loadConfigFromEnv(newDiscardLogger())
			if err != nil {
				t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
			}
			tt.assert(t, cfg)
		})
	}
}

// TestLoadConfigFromEnvSeverityAliasDefaultNonGCP ensures aliases are disabled by default outside GCP.
func TestLoadConfigFromEnvSeverityAliasDefaultNonGCP(t *testing.T) {
	clearHandlerEnv(t)

	cfg, err := loadConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
	}
	if cfg.UseShortSeverityNames {
		t.Fatalf("cfg.UseShortSeverityNames = true, want false when no GCP runtime detected")
	}
}

// TestLoadConfigFromEnvSeverityAliasDefaultGCP ensures aliases stay enabled by default when GCP is detected.
func TestLoadConfigFromEnvSeverityAliasDefaultGCP(t *testing.T) {
	clearHandlerEnv(t)
	t.Setenv("K_SERVICE", "svc")
	t.Setenv("K_REVISION", "rev")
	t.Setenv("K_CONFIGURATION", "cfg")
	t.Setenv(envGoogleProject, "project-id")

	cfg, err := loadConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
	}
	if !cfg.UseShortSeverityNames {
		t.Fatalf("cfg.UseShortSeverityNames = false, want true when GCP runtime detected")
	}
}

// TestLoadConfigFromEnvTimeDefaultNonGCP ensures timestamps are emitted by default outside managed GCP runtimes.
func TestLoadConfigFromEnvTimeDefaultNonGCP(t *testing.T) {
	clearHandlerEnv(t)

	cfg, err := loadConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
	}
	if !cfg.EmitTimeField {
		t.Fatalf("cfg.EmitTimeField = false, want true outside managed GCP runtimes")
	}
}

// TestLoadConfigFromEnvTimeDefaultCloudRun ensures timestamps are omitted by default on Cloud Run.
func TestLoadConfigFromEnvTimeDefaultCloudRun(t *testing.T) {
	clearHandlerEnv(t)
	t.Setenv("K_SERVICE", "svc")
	t.Setenv("K_REVISION", "rev")
	t.Setenv("K_CONFIGURATION", "cfg")

	cfg, err := loadConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
	}
	if cfg.EmitTimeField {
		t.Fatalf("cfg.EmitTimeField = true, want false on Cloud Run by default")
	}
}

// TestLoadConfigFromEnvStackTraceLevel confirms the stack trace level override is applied.
func TestLoadConfigFromEnvStackTraceLevel(t *testing.T) {
	clearHandlerEnv(t)
	t.Setenv(envLogStackLevel, "notice")

	cfg, err := loadConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
	}
	if cfg.StackTraceLevel != slog.Level(LevelNotice) {
		t.Fatalf("cfg.StackTraceLevel = %v, want %v", cfg.StackTraceLevel, slog.Level(LevelNotice))
	}
}

// TestLoadConfigFromEnvTargets exercises environment target resolution and validation.
func TestLoadConfigFromEnvTargets(t *testing.T) {
	t.Run("stdout", func(t *testing.T) {
		clearHandlerEnv(t)
		t.Setenv(envTarget, "stdout")

		cfg, err := loadConfigFromEnv(newDiscardLogger())
		if err != nil {
			t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
		}
		if cfg.Writer != os.Stdout {
			t.Fatalf("cfg.Writer = %v, want os.Stdout", cfg.Writer)
		}
		if !cfg.writerExternallyOwned {
			t.Fatalf("cfg.writerExternallyOwned = false, want true")
		}
	})

	t.Run("stderr", func(t *testing.T) {
		clearHandlerEnv(t)
		t.Setenv(envTarget, "stderr")

		cfg, err := loadConfigFromEnv(newDiscardLogger())
		if err != nil {
			t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
		}
		if cfg.Writer != os.Stderr {
			t.Fatalf("cfg.Writer = %v, want os.Stderr", cfg.Writer)
		}
		if !cfg.writerExternallyOwned {
			t.Fatalf("cfg.writerExternallyOwned = false, want true")
		}
	})

	t.Run("file", func(t *testing.T) {
		clearHandlerEnv(t)
		path := filepath.Join(t.TempDir(), "app.log")
		t.Setenv(envTarget, "file:"+path)

		cfg, err := loadConfigFromEnv(newDiscardLogger())
		if err != nil {
			t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
		}
		if cfg.FilePath != path {
			t.Fatalf("cfg.FilePath = %q, want %q", cfg.FilePath, path)
		}
		if cfg.Writer != nil {
			t.Fatalf("cfg.Writer = %v, want nil", cfg.Writer)
		}
		if cfg.writerExternallyOwned {
			t.Fatalf("cfg.writerExternallyOwned = true, want false")
		}
	})

	t.Run("invalid", func(t *testing.T) {
		clearHandlerEnv(t)
		t.Setenv(envTarget, "unknown")

		if _, err := loadConfigFromEnv(newDiscardLogger()); err == nil {
			t.Fatalf("loadConfigFromEnv() error = nil, want ErrInvalidRedirectTarget")
		}
	})

	t.Run("empty_file", func(t *testing.T) {
		clearHandlerEnv(t)
		t.Setenv(envTarget, "file:")

		if _, err := loadConfigFromEnv(newDiscardLogger()); err == nil {
			t.Fatalf("loadConfigFromEnv() error = nil, want ErrInvalidRedirectTarget")
		}
	})
}

// TestParseBoolEnvCoversValidation exercises valid, invalid, and blank values.
func TestParseBoolEnvCoversValidation(t *testing.T) {
	t.Parallel()

	logger := newDiscardLogger()
	if !parseBoolEnv("true", false, logger) {
		t.Fatalf("parseBoolEnv(true) = false, want true")
	}
	if parseBoolEnv("invalid", true, logger) != true {
		t.Fatalf("parseBoolEnv(invalid) should keep current value")
	}
	if parseBoolEnv("", true, logger) != true {
		t.Fatalf("parseBoolEnv blank should keep current value")
	}
}

// TestParseLevelEnvCoversAliases exercises numeric and named inputs.
func TestParseLevelEnvCoversAliases(t *testing.T) {
	t.Parallel()

	logger := newDiscardLogger()
	tests := []struct {
		input string
		want  slog.Level
	}{
		{input: "info", want: slog.LevelInfo},
		{input: "warn", want: slog.LevelWarn},
		{input: "warning", want: slog.LevelWarn},
		{input: "error", want: slog.LevelError},
		{input: "default", want: slog.Level(LevelDefault)},
		{input: "debug", want: slog.LevelDebug},
		{input: "NOTICE", want: slog.Level(LevelNotice)},
		{input: "critical", want: slog.Level(LevelCritical)},
		{input: "alert", want: slog.Level(LevelAlert)},
		{input: "emergency", want: slog.Level(LevelEmergency)},
		{input: "17", want: slog.Level(17)},
	}
	for _, tt := range tests {
		if got := parseLevelEnv(tt.input, slog.LevelInfo, logger); got != tt.want {
			t.Fatalf("parseLevelEnv(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}

	if got := parseLevelEnv("invalid", slog.LevelWarn, logger); got != slog.LevelWarn {
		t.Fatalf("parseLevelEnv invalid should retain current level")
	}
}

// TestCachedConfigFromEnvCachesResults ensures the first successful load is reused.
func TestCachedConfigFromEnvCachesResults(t *testing.T) {
	clearHandlerEnv(t)
	t.Cleanup(resetHandlerConfigCache)

	t.Setenv(envLogLevel, "error")
	cfg, err := cachedConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("cachedConfigFromEnv() returned %v", err)
	}
	if cfg.Level != slog.LevelError {
		t.Fatalf("cached level = %v, want %v", cfg.Level, slog.LevelError)
	}

	// Changing the environment without resetting the cache should not affect the result.
	t.Setenv(envLogLevel, "debug")
	cfgAgain, err := cachedConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("cachedConfigFromEnv() second call returned %v", err)
	}
	if cfgAgain.Level != slog.LevelError {
		t.Fatalf("cached level after env change = %v, want %v", cfgAgain.Level, slog.LevelError)
	}
}

// TestIsStdStream ensures stdout/stderr detection works and rejects other writers.
func TestIsStdStream(t *testing.T) {
	t.Parallel()

	if !isStdStream(os.Stdout) {
		t.Fatalf("expected stdout to be detected")
	}
	if !isStdStream(os.Stderr) {
		t.Fatalf("expected stderr to be detected")
	}
	if isStdStream(io.Discard) {
		t.Fatalf("io.Discard should not be treated as std stream")
	}
	var nilFile *os.File
	if isStdStream(nilFile) {
		t.Fatalf("nil *os.File should not be reported as std stream")
	}
}

// TestLogDiagnosticHandlesNilLogger ensures nil loggers are ignored.
func TestLogDiagnosticHandlesNilLogger(t *testing.T) {
	t.Parallel()

	logDiagnostic(nil, slog.LevelInfo, "message")

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{AddSource: false}))
	logDiagnostic(logger, slog.LevelInfo, "hello", slog.String("k", "v"))
	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &payload); err != nil {
		t.Fatalf("json.Unmarshal() returned %v", err)
	}
	if got := payload["msg"]; got != "hello" {
		t.Fatalf("payload msg = %v, want %q", got, "hello")
	}
}

// TestNewTraceDiagnosticsFallbacks ensures a default logger is created when the global logger is nil.
func TestNewTraceDiagnosticsFallbacks(t *testing.T) {
	original := traceDiagnosticLogger
	t.Cleanup(func() {
		traceDiagnosticLogger = original
	})
	traceDiagnosticLogger = nil

	if td := newTraceDiagnostics(TraceDiagnosticsOff); td != nil {
		t.Fatalf("newTraceDiagnostics(off) = %v, want nil", td)
	}

	td := newTraceDiagnostics(TraceDiagnosticsWarnOnce)
	if td == nil {
		t.Fatalf("newTraceDiagnostics returned nil in warn once mode")
	}
	if td.logger == nil {
		t.Fatalf("trace diagnostics logger should fall back to non-nil instance")
	}
}

// TestTraceDiagnosticsWarnUnknownProjectLogsOnce ensures warnings are emitted at most once per instance.
func TestTraceDiagnosticsWarnUnknownProjectLogsOnce(t *testing.T) {
	t.Parallel()

	logger := &diagRecorder{}
	td := &traceDiagnostics{mode: TraceDiagnosticsWarnOnce, logger: logger}
	td.warnUnknownProject()
	td.warnUnknownProject()

	if len(logger.messages) != 1 {
		t.Fatalf("warnUnknownProject logged %d messages, want 1", len(logger.messages))
	}

	silent := &traceDiagnostics{mode: TraceDiagnosticsOff, logger: logger}
	silent.warnUnknownProject()
	if len(logger.messages) != 1 {
		t.Fatalf("TraceDiagnosticsOff should not log warnings")
	}
}

// TestParseTraceDiagnosticsEnvCoversModes validates environment inputs and defaults.
func TestParseTraceDiagnosticsEnvCoversModes(t *testing.T) {
	t.Parallel()

	logger := newDiscardLogger()
	tests := []struct {
		name    string
		value   string
		current TraceDiagnosticsMode
		want    TraceDiagnosticsMode
	}{
		{"blank", "", TraceDiagnosticsWarnOnce, TraceDiagnosticsWarnOnce},
		{"off_alias", "disable", TraceDiagnosticsWarnOnce, TraceDiagnosticsOff},
		{"warn_alias", "warn-once", TraceDiagnosticsOff, TraceDiagnosticsWarnOnce},
		{"strict", "strict", TraceDiagnosticsWarnOnce, TraceDiagnosticsStrict},
		{"invalid", "mystery", TraceDiagnosticsStrict, TraceDiagnosticsStrict},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseTraceDiagnosticsEnv(tt.value, tt.current, logger); got != tt.want {
				t.Fatalf("parseTraceDiagnosticsEnv(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

// TestLoadConfigFromEnvHelpersFallback exercises the helper accessors and defaulting paths.
func TestLoadConfigFromEnvHelpersFallback(t *testing.T) {
	originalLoader := getLoadConfigFromEnv()
	defer setLoadConfigFromEnv(originalLoader)

	setLoadConfigFromEnv(nil)
	if fn := getLoadConfigFromEnv(); fn == nil {
		t.Fatalf("getLoadConfigFromEnv returned nil after nil setter")
	}

	loadConfigFromEnvFunc = atomic.Value{}
	if fn := getLoadConfigFromEnv(); fn == nil {
		t.Fatalf("getLoadConfigFromEnv returned nil after zeroing atomic value")
	}

	setLoadConfigFromEnv(originalLoader)
}

// TestCachedConfigFromEnvInvokesRaceHook simulates a concurrent cache fill to ensure the hook fires.
func TestCachedConfigFromEnvInvokesRaceHook(t *testing.T) {
	clearHandlerEnv(t)
	resetHandlerConfigCache()

	originalLoader := getLoadConfigFromEnv()
	originalHook := getCachedConfigRaceHook()
	t.Cleanup(func() {
		setLoadConfigFromEnv(originalLoader)
		setCachedConfigRaceHook(originalHook)
		resetHandlerConfigCache()
	})

	raceWinner := &handlerConfig{Level: slog.LevelWarn}
	setLoadConfigFromEnv(func(logger *slog.Logger) (handlerConfig, error) {
		handlerEnvConfigCache.Store(raceWinner)
		return handlerConfig{Level: slog.LevelInfo}, nil
	})

	hookCalled := false
	setCachedConfigRaceHook(func() {
		hookCalled = true
	})

	cfg, err := cachedConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("cachedConfigFromEnv() returned %v, want nil", err)
	}
	if !hookCalled {
		t.Fatalf("cachedConfigRaceHook was not invoked")
	}
	if cfg.Level != raceWinner.Level {
		t.Fatalf("cfg.Level = %v, want %v from existing cache", cfg.Level, raceWinner.Level)
	}
}

type diagRecorder struct {
	messages []string
}

// Printf appends formatted messages for verification in tests.
func (l *diagRecorder) Printf(format string, args ...any) {
	l.messages = append(l.messages, fmt.Sprintf(format, args...))
}
