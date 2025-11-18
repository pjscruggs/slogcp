package slogcp

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// newDiscardLogger returns a test logger that discards all output.
func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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
