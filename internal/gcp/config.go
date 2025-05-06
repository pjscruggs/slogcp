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

package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"cloud.google.com/go/compute/metadata"
)

// LogTarget defines the destination for log output.
// This type is defined internally and aliased publicly in the parent package.
type LogTarget int

const (
	// LogTargetGCP directs logs to the Google Cloud Logging API (default).
	LogTargetGCP LogTarget = iota
	// LogTargetStdout directs logs to standard output as structured JSON.
	LogTargetStdout
	// LogTargetStderr directs logs to standard error as structured JSON.
	LogTargetStderr
)

// String returns the string representation of the LogTarget.
func (lt LogTarget) String() string {
	switch lt {
	case LogTargetGCP:
		return "gcp"
	case LogTargetStdout:
		return "stdout"
	case LogTargetStderr:
		return "stderr"
	default:
		return fmt.Sprintf("unknown(%d)", int(lt))
	}
}

// Config holds configuration values for the slogcp logger.
// These values are typically loaded from environment variables or set via
// functional options during logger initialization.
type Config struct {
	// InitialLevel specifies the minimum level of log entries that will be processed.
	InitialLevel slog.Level

	// AddSource determines whether source code location (file, line, function)
	// is added to log entries. Enabling this incurs a performance cost.
	AddSource bool

	// ProjectID is the Google Cloud Project ID used by the Cloud Logging client.
	// This field is required for operation when LogTarget is LogTargetGCP.
	ProjectID string

	// ServiceName is the name of the Cloud Run service, typically read from K_SERVICE.
	// It may be used for associating logs with the service resource.
	ServiceName string

	// RevisionName is the name of the Cloud Run revision, typically read from K_REVISION.
	// It may be used for associating logs with the service resource.
	RevisionName string

	// StackTraceEnabled determines whether stack traces should be captured
	// and included for logs at or above StackTraceLevel.
	StackTraceEnabled bool

	// StackTraceLevel specifies the minimum level at which stack traces
	// should be captured if StackTraceEnabled is true.
	StackTraceLevel slog.Level

	// LogTarget specifies the destination for log output (GCP, stdout, stderr).
	LogTarget LogTarget

	// ReplaceAttrFunc allows custom attribute replacement or masking
	ReplaceAttrFunc func([]string, slog.Attr) slog.Attr
}

// Environment variable names used for configuration.
const (
	envLogLevel          = "LOG_LEVEL"               // Sets the initial logging level (e.g., "DEBUG", "-4", "INFO", "0").
	envLogSourceLocation = "LOG_SOURCE_LOCATION"     // Enables source location logging if "true" or "1".
	envProjectID         = "GOOGLE_CLOUD_PROJECT"    // Specifies the GCP project ID.
	envService           = "K_SERVICE"               // Cloud Run service name.
	envRevision          = "K_REVISION"              // Cloud Run revision name.
	envStackTraceEnabled = "LOG_STACK_TRACE_ENABLED" // Enables stack trace logging if "true" or "1".
	envStackTraceLevel   = "LOG_STACK_TRACE_LEVEL"   // Sets the minimum level for stack traces (e.g., "ERROR" or "8").
	envLogTarget         = "SLOGCP_LOG_TARGET"       // Sets the log destination ("gcp", "stdout", "stderr").

	// Default values used if environment variables are missing or invalid.
	defaultLevel           = slog.LevelInfo  // Default logging level is INFO.
	defaultAddSource       = false           // Source location is disabled by default.
	defaultStackTrace      = false           // Stack traces disabled by default.
	defaultStackTraceLevel = slog.LevelError // Stack traces only for ERROR+ by default.
	defaultLogTarget       = LogTargetGCP    // Default log target is GCP.
)

// LoadConfig reads configuration from environment variables, applying defaults
// for unspecified or invalid values.
//
// It determines the ProjectID by first checking the GOOGLE_CLOUD_PROJECT
// environment variable. If that is not set, it attempts to retrieve the project ID
// from the GCP metadata server (if running in a GCP environment).
// It returns an error wrapping ErrProjectIDMissing if a project ID cannot be
// determined through either method and the target is GCP.
func LoadConfig() (Config, error) {
	projectID := os.Getenv(envProjectID)
	logTarget := parseLogTarget(os.Getenv(envLogTarget))

	// If the environment variable is not set and the target requires GCP,
	// attempt to fetch from metadata server.
	if projectID == "" && logTarget == LogTargetGCP {
		// Check if running in a GCP environment where metadata server is available.
		if metadata.OnGCE() {
			ctx := context.Background()
			metadataProjectID, err := metadata.ProjectIDWithContext(ctx)
			// Use the metadata project ID if successfully retrieved.
			if err == nil && metadataProjectID != "" {
				projectID = metadataProjectID
				// Log informational message to stderr as logger isn't ready yet.
				fmt.Fprintf(os.Stderr, "[slogcp config] INFO: Using project ID %q from metadata server\n", projectID)
			} else if err != nil {
				// Log failure to retrieve from metadata but continue, as env var might be set later or explicitly.
				fmt.Fprintf(os.Stderr, "[slogcp config] WARNING: Failed to get project ID from metadata server: %v\n", err)
			}
		}
	}

	// If no project ID could be determined and the target is GCP, return an error.
	if projectID == "" && logTarget == LogTargetGCP {
		return Config{}, fmt.Errorf("slogcp: %s environment variable is not set and metadata lookup failed (required for GCP target): %w", envProjectID, ErrProjectIDMissing)
	}

	// Populate the config struct using parsed environment variables or defaults.
	cfg := Config{
		InitialLevel:      parseLevel(os.Getenv(envLogLevel)),
		AddSource:         parseBool(os.Getenv(envLogSourceLocation), defaultAddSource, envLogSourceLocation),
		ProjectID:         projectID, // May be empty if target is not GCP
		ServiceName:       os.Getenv(envService),
		RevisionName:      os.Getenv(envRevision),
		StackTraceEnabled: parseBool(os.Getenv(envStackTraceEnabled), defaultStackTrace, envStackTraceEnabled),
		StackTraceLevel:   parseLevelWithDefault(os.Getenv(envStackTraceLevel), defaultStackTraceLevel, envStackTraceLevel),
		LogTarget:         logTarget,
	}

	return cfg, nil
}

// parseLogTarget interprets the log target string from the environment.
func parseLogTarget(targetStr string) LogTarget {
	switch strings.ToLower(strings.TrimSpace(targetStr)) {
	case "stdout":
		return LogTargetStdout
	case "stderr":
		return LogTargetStderr
	case "gcp":
		return LogTargetGCP
	default:
		// Empty string or unrecognized value defaults to GCP.
		return defaultLogTarget
	}
}

// parseLevel interprets the log level string from the environment, falling back
// to the package default (slog.LevelInfo) if empty or invalid.
// Supports both named level strings (e.g., "DEBUG", "ERROR") and numerical
// values (e.g., "-4", "8") that correspond directly to slog.Level integers.
func parseLevel(levelStr string) slog.Level {
	return parseLevelWithDefault(levelStr, defaultLevel, envLogLevel)
}

// parseLevelWithDefault interprets the log level string from the environment,
// falling back to a specified default level if the input is empty or invalid.
// It logs a warning to stderr if the input string is non-empty but invalid.
// Supports both named level strings (e.g., "DEBUG", "ERROR") and numerical
// values (e.g., "-4", "8") that correspond directly to slog.Level integers.
func parseLevelWithDefault(levelStr string, defaultLvl slog.Level, envVarName string) slog.Level {
	trimmedLevelStr := strings.ToLower(strings.TrimSpace(levelStr))
	if trimmedLevelStr == "" {
		return defaultLvl // Use provided default if empty.
	}

	// Map known level strings (case-insensitive) to slog.Level values.
	// Use the internal level constants defined in severity.go
	switch trimmedLevelStr {
	case "default":
		return internalLevelDefault
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "notice":
		return internalLevelNotice
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "critical":
		return internalLevelCritical
	case "alert":
		return internalLevelAlert
	case "emergency":
		return internalLevelEmergency
	default:
		// Try to parse as a numerical value
		if levelVal, err := strconv.Atoi(trimmedLevelStr); err == nil {
			return slog.Level(levelVal)
		}

		// Log warning for unrecognized level string.
		fmt.Fprintf(os.Stderr, "[slogcp config] WARNING: Invalid %s value %q, defaulting to %v\n", envVarName, levelStr, defaultLvl)
		return defaultLvl
	}
}

// parseBool interprets a boolean string from the environment, falling back to a
// specified default value if the input is empty or invalid.
// It logs a warning to stderr if the input string is non-empty but invalid.
func parseBool(boolStr string, defaultVal bool, envVarName string) bool {
	trimmedBoolStr := strings.TrimSpace(boolStr)
	if trimmedBoolStr == "" {
		return defaultVal // Use provided default if empty.
	}

	// Attempt to parse the string as a boolean.
	val, err := strconv.ParseBool(trimmedBoolStr)
	if err != nil {
		// Log warning for unrecognized boolean string.
		fmt.Fprintf(os.Stderr, "[slogcp config] WARNING: Invalid %s value %q, defaulting to %v\n", envVarName, boolStr, defaultVal)
		return defaultVal
	}
	return val
}
