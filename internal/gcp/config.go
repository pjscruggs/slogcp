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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
)

// LogTarget defines the destination for log output. It determines where logs
// will be sent: to Google Cloud Logging, stdout, stderr, or a file.
type LogTarget int

const (
	// LogTargetGCP directs logs to the Google Cloud Logging API (default).
	LogTargetGCP LogTarget = iota
	// LogTargetStdout directs logs to standard output as structured JSON.
	LogTargetStdout
	// LogTargetStderr directs logs to standard error as structured JSON.
	LogTargetStderr
	// LogTargetFile directs logs to a specified file as structured JSON.
	LogTargetFile
)

// String returns the string representation of the LogTarget.
// This implements the fmt.Stringer interface for human-readable output.
func (lt LogTarget) String() string {
	switch lt {
	case LogTargetGCP:
		return "gcp"
	case LogTargetStdout:
		return "stdout"
	case LogTargetStderr:
		return "stderr"
	case LogTargetFile:
		return "file"
	default:
		return fmt.Sprintf("unknown(%d)", int(lt))
	}
}

// Environment variable names used for configuration.
// These can be set in the runtime environment to control logger behavior.
const (
	// SLOGCP specific config
	envLogLevel                = "LOG_LEVEL"                      // Minimum log level to output
	envLogSourceLocation       = "LOG_SOURCE_LOCATION"            // Whether to include source file info
	envStackTraceEnabled       = "LOG_STACK_TRACE_ENABLED"        // Whether to capture stack traces
	envStackTraceLevel         = "LOG_STACK_TRACE_LEVEL"          // Minimum level for stack traces
	envLogTarget               = "SLOGCP_LOG_TARGET"              // Where logs should be sent
	envRedirectAsJSONTarget    = "SLOGCP_REDIRECT_AS_JSON_TARGET" // "stdout", "stderr", "file:/path/to/file.log"
	envSlogcpProjectIDOverride = "SLOGCP_PROJECT_ID"              // Override GCP project ID

	// GCP Client passthroughs
	envGCPParent       = "SLOGCP_GCP_PARENT"        // GCP resource parent
	envGCPClientScopes = "SLOGCP_GCP_CLIENT_SCOPES" // OAuth scopes for GCP client
	envGCPLogID        = "SLOGCP_GCP_LOG_ID"        // Cloud Logging log ID
	// GOOGLE_CLOUD_PROJECT is handled implicitly

	// GCP Logger passthroughs
	envGCPCommonLabelsJSON     = "SLOGCP_GCP_COMMON_LABELS_JSON"      // JSON map of common labels
	envGCPCommonLabelPrefix    = "SLOGCP_GCP_CL_"                     // Prefix for common label env vars
	envGCPResourceType         = "SLOGCP_GCP_RESOURCE_TYPE"           // Monitored resource type
	envGCPResourceLabelPrefix  = "SLOGCP_GCP_RL_"                     // Prefix for resource label env vars
	envGCPConcurrentWriteLimit = "SLOGCP_GCP_CONCURRENT_WRITE_LIMIT"  // Max concurrent requests
	envGCPDelayThresholdMS     = "SLOGCP_GCP_DELAY_THRESHOLD_MS"      // Buffer time in milliseconds
	envGCPEntryCountThreshold  = "SLOGCP_GCP_ENTRY_COUNT_THRESHOLD"   // Max entries before flush
	envGCPEntryByteThreshold   = "SLOGCP_GCP_ENTRY_BYTE_THRESHOLD"    // Max bytes before flush
	envGCPEntryByteLimit       = "SLOGCP_GCP_ENTRY_BYTE_LIMIT"        // Max bytes per entry
	envGCPBufferedByteLimit    = "SLOGCP_GCP_BUFFERED_BYTE_LIMIT"     // Memory limit for buffering
	envGCPContextFuncTimeoutMS = "SLOGCP_GCP_CONTEXT_FUNC_TIMEOUT_MS" // Context timeout
	envGCPPartialSuccess       = "SLOGCP_GCP_PARTIAL_SUCCESS"         // Accept partial batch success
)

// Default values used if environment variables are missing or invalid.
const (
	slogcpDefaultLogLevel             = slog.LevelInfo
	slogcpDefaultAddSource            = false
	slogcpDefaultStackTraceEnabled    = false
	slogcpDefaultStackTraceLevel      = slog.LevelError
	slogcpDefaultLogTarget            = LogTargetGCP
	EnvLogTarget                      = envLogTarget
	EnvRedirectAsJSONTarget           = envRedirectAsJSONTarget
	EnvGCPLogID                       = envGCPLogID
	slogcpDefaultGCPBufferedByteLimit = 100 * 1024 * 1024 // 100 MiB
	slogcpDefaultGCPLogID             = "app"
	defaultClientInitTimeout          = 10 * time.Second
)

// Config holds all resolved configuration values after processing defaults,
// environment variables, and programmatic options. This is the central
// configuration struct used throughout the library.
type Config struct {
	InitialLevel      slog.Level // Minimum log level to emit
	AddSource         bool       // Include source file location in logs
	StackTraceEnabled bool       // Capture stack traces for errors
	StackTraceLevel   slog.Level // Minimum level for stack trace capture
	LogTarget         LogTarget  // Where to send logs

	// RedirectWriter is the io.Writer used when the LogTarget is not GCP.
	// If logging to a file managed by slogcp (indicated by a non-empty
	// OpenedFilePath), this field will hold an internal *gcp.SwitchableWriter.
	// If a user provided a writer directly (e.g., a lumberjack.Logger via
	// slogcp.WithRedirectWriter), this field will hold that user-provided writer.
	RedirectWriter io.Writer

	// ClosableUserWriter stores the user-provided io.Writer if it implements
	// io.Closer. This allows slogcp.Logger.Close() to correctly
	// close writers such as lumberjack.Logger instances that are passed
	// via slogcp.WithRedirectWriter.
	ClosableUserWriter io.Closer

	OpenedFilePath  string                              // Path of the file slogcp opened itself (if configured via WithRedirectToFile).
	ReplaceAttrFunc func([]string, slog.Attr) slog.Attr // For attribute transformation
	Middlewares     []func(slog.Handler) slog.Handler   // Handler middleware chain
	InitialAttrs    []slog.Attr                         // Initial attributes set by options
	InitialGroup    string                              // Initial group name set by options

	ProjectID string // Final resolved project ID - REQUIRED for trace formatting if trace exists
	Parent    string // Final resolved parent - REQUIRED for GCP target
	GCPLogID  string // Final resolved log ID - defaults to "app" if unset

	ClientScopes      []string    // nil means use GCP default
	ClientOnErrorFunc func(error) // nil means use slogcp default (stderr)

	GCPCommonLabels          map[string]string                // nil means use GCP default (none)
	GCPMonitoredResource     *mrpb.MonitoredResource          // nil means use GCP default (auto-detect)
	GCPConcurrentWriteLimit  *int                             // nil means use GCP default (1)
	GCPDelayThreshold        *time.Duration                   // nil means use GCP default (1s)
	GCPEntryCountThreshold   *int                             // nil means use GCP default (1000)
	GCPEntryByteThreshold    *int                             // nil means use GCP default (8MiB)
	GCPEntryByteLimit        *int                             // nil means use GCP default (0 = no limit)
	GCPBufferedByteLimit     *int                             // nil means use *slogcp* default (100MiB)
	GCPContextFunc           func() (context.Context, func()) // nil means use GCP default (context.Background)
	GCPDefaultContextTimeout time.Duration                    // Used only if GCPContextFunc is nil & env var/option set
	GCPPartialSuccess        *bool                            // nil means use GCP default (false)
}

// LoadConfig loads configuration from environment variables, applying defaults
// for unspecified or invalid values. This provides the base configuration layer
// before programmatic options are applied.
//
// It resolves settings from multiple sources in this order of precedence:
//  1. Hard-coded defaults
//  2. Environment variables
//  3. GCP metadata server (for project ID, if running on GCP)
//
// If LogTarget is GCP and no project ID or parent can be resolved from any source,
// it returns ErrProjectIDMissing. Other errors are returned for invalid redirect
// targets or file operations.
func LoadConfig() (Config, error) {
	// Initialize with slogcp's hardcoded defaults
	cfg := Config{
		InitialLevel:         slogcpDefaultLogLevel,
		AddSource:            slogcpDefaultAddSource,
		StackTraceEnabled:    slogcpDefaultStackTraceEnabled,
		StackTraceLevel:      slogcpDefaultStackTraceLevel,
		LogTarget:            slogcpDefaultLogTarget,
		GCPLogID:             slogcpDefaultGCPLogID,
		GCPBufferedByteLimit: func() *int { b := slogcpDefaultGCPBufferedByteLimit; return &b }(),
	}

	// Process the log target and redirect target
	envTargetStr := os.Getenv(envLogTarget)
	cfg.LogTarget = parseLogTargetEnv(envTargetStr, slogcpDefaultLogTarget)

	envRedirectTargetStr := os.Getenv(envRedirectAsJSONTarget)
	// Only process SLOGCP_REDIRECT_AS_JSON_TARGET if LogTarget is not GCP.
	// If LogTarget is GCP, redirect settings are ignored.
	if cfg.LogTarget != LogTargetGCP {
		switch {
		case strings.HasPrefix(envRedirectTargetStr, "file:"):
			filePath := strings.TrimPrefix(envRedirectTargetStr, "file:")
			if filePath == "" {
				return Config{}, fmt.Errorf("invalid redirect target: file path missing in %s=%s", envRedirectAsJSONTarget, envRedirectTargetStr)
			}
			// Store the path; actual file opening is deferred to initializeLogger.
			cfg.OpenedFilePath = filePath
			cfg.LogTarget = LogTargetFile // Ensure LogTarget is File if "file:" prefix is used.
		case envRedirectTargetStr == "stdout":
			cfg.RedirectWriter = os.Stdout
			cfg.LogTarget = LogTargetStdout
		case envRedirectTargetStr == "stderr":
			cfg.RedirectWriter = os.Stderr
			cfg.LogTarget = LogTargetStderr
		case envRedirectTargetStr == "":
			// If SLOGCP_REDIRECT_AS_JSON_TARGET is empty, but SLOGCP_LOG_TARGET indicated
			// stdout or stderr, set the writer accordingly.
			switch cfg.LogTarget {
			case LogTargetStdout:
				cfg.RedirectWriter = os.Stdout
			case LogTargetStderr:
				cfg.RedirectWriter = os.Stderr
			}
			// If LogTarget was File but no path given via SLOGCP_REDIRECT_AS_JSON_TARGET,
			// this will be an error, caught later in applyProgrammaticOptions or initializeLogger
			// if no programmatic option provides a path or writer.
		default:
			// An unrecognized value for SLOGCP_REDIRECT_AS_JSON_TARGET.
			return Config{}, fmt.Errorf("invalid redirect target specified in %s: %s", envRedirectAsJSONTarget, envRedirectTargetStr)
		}
	}

	// Resolve GCP Log ID from environment variable if set
	// Otherwise, it remains as the default.
	if envLogID := os.Getenv(envGCPLogID); envLogID != "" {
		cfg.GCPLogID = envLogID
	}

	// Resolve project ID and parent from environment or metadata server
	// Order of precedence: SLOGCP_GCP_PARENT > SLOGCP_PROJECT_ID > GOOGLE_CLOUD_PROJECT > metadata
	envParent := os.Getenv(envGCPParent)
	envSlogcpProjectIDOverride := os.Getenv(envSlogcpProjectIDOverride)
	envGoogleCloudProject := os.Getenv("GOOGLE_CLOUD_PROJECT")

	if envParent != "" {
		cfg.Parent = envParent
		if strings.HasPrefix(envParent, "projects/") {
			cfg.ProjectID = strings.TrimPrefix(envParent, "projects/")
		}
	} else if envSlogcpProjectIDOverride != "" {
		cfg.ProjectID = envSlogcpProjectIDOverride
		cfg.Parent = "projects/" + envSlogcpProjectIDOverride
	} else if envGoogleCloudProject != "" {
		cfg.ProjectID = envGoogleCloudProject
		cfg.Parent = "projects/" + envGoogleCloudProject
	} else if cfg.LogTarget == LogTargetGCP {
		// Try metadata server as last resort for GCP target
		if metadata.OnGCE() {
			ctx := context.Background() // Use Background context for metadata calls
			id, err := metadata.ProjectIDWithContext(ctx)
			if err == nil && id != "" {
				cfg.ProjectID = id
				cfg.Parent = "projects/" + id
				fmt.Fprintf(os.Stderr, "[slogcp config] INFO: Using project ID %q from metadata server\n", id)
			} else if err != nil {
				fmt.Fprintf(os.Stderr, "[slogcp config] WARNING: OnGCE but failed to get project ID from metadata: %v\n", err)
			}
		}
	}

	// Process core logging settings
	cfg.InitialLevel = parseLevelEnv(os.Getenv(envLogLevel), cfg.InitialLevel)
	cfg.AddSource = parseBoolEnv(os.Getenv(envLogSourceLocation), cfg.AddSource)
	cfg.StackTraceEnabled = parseBoolEnv(os.Getenv(envStackTraceEnabled), cfg.StackTraceEnabled)
	cfg.StackTraceLevel = parseLevelEnv(os.Getenv(envStackTraceLevel), cfg.StackTraceLevel)

	// Process client scopes (comma-separated list)
	if val := os.Getenv(envGCPClientScopes); val != "" {
		scopes := strings.Split(val, ",")
		trimmedScopes := make([]string, 0, len(scopes))
		for _, s := range scopes {
			trimmed := strings.TrimSpace(s)
			if trimmed != "" {
				trimmedScopes = append(trimmedScopes, trimmed)
			}
		}
		if len(trimmedScopes) > 0 {
			cfg.ClientScopes = trimmedScopes
		}
	}

	// Process Common Labels from both JSON and environment variables
	// Labels from prefix env vars override those from JSON
	labels := make(map[string]string)
	if jsonStr := os.Getenv(envGCPCommonLabelsJSON); jsonStr != "" {
		if err := json.Unmarshal([]byte(jsonStr), &labels); err != nil {
			fmt.Fprintf(os.Stderr, "[slogcp config] WARNING: Failed to parse JSON from %s: %v\n", envGCPCommonLabelsJSON, err)
		}
	}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, envGCPCommonLabelPrefix) {
			parts := strings.SplitN(e, "=", 2)
			key := strings.TrimPrefix(parts[0], envGCPCommonLabelPrefix)
			if key != "" && len(parts) == 2 {
				labels[key] = parts[1] // Prefix value overrides JSON value
			}
		}
	}
	if len(labels) > 0 {
		cfg.GCPCommonLabels = labels
	}

	// Process Monitored Resource settings
	envResourceType := os.Getenv(envGCPResourceType)
	if envResourceType != "" {
		resourceLabels := make(map[string]string)
		prefix := envGCPResourceLabelPrefix
		for _, e := range os.Environ() {
			if strings.HasPrefix(e, prefix) {
				parts := strings.SplitN(e, "=", 2)
				key := strings.TrimPrefix(parts[0], prefix)
				if key != "" && len(parts) == 2 {
					resourceLabels[key] = parts[1]
				}
			}
		}
		cfg.GCPMonitoredResource = &mrpb.MonitoredResource{Type: envResourceType, Labels: resourceLabels}
	}

	// Process Cloud Logging client buffering and behavior options
	cfg.GCPConcurrentWriteLimit = parseIntPtrEnv(os.Getenv(envGCPConcurrentWriteLimit))
	cfg.GCPDelayThreshold = parseDurationPtrEnvMS(os.Getenv(envGCPDelayThresholdMS))
	cfg.GCPEntryCountThreshold = parseIntPtrEnv(os.Getenv(envGCPEntryCountThreshold))
	cfg.GCPEntryByteThreshold = parseIntPtrEnv(os.Getenv(envGCPEntryByteThreshold))
	cfg.GCPEntryByteLimit = parseIntPtrEnv(os.Getenv(envGCPEntryByteLimit))
	if valStr := os.Getenv(envGCPBufferedByteLimit); valStr != "" {
		cfg.GCPBufferedByteLimit = parseIntPtrEnv(valStr) // Override slogcp default if set
	}

	// Process context timeout for background API operations
	if valStr := os.Getenv(envGCPContextFuncTimeoutMS); valStr != "" {
		if ms, err := strconv.Atoi(valStr); err == nil && ms > 0 {
			cfg.GCPDefaultContextTimeout = time.Duration(ms) * time.Millisecond
		} else if err != nil {
			fmt.Fprintf(os.Stderr, "[slogcp config] WARNING: Invalid integer value for %s: %s\n", envGCPContextFuncTimeoutMS, valStr)
		}
	}
	cfg.GCPPartialSuccess = parseBoolPtrEnv(os.Getenv(envGCPPartialSuccess))

	// Check for project ID missing error condition *after* parsing all potential sources
	if cfg.LogTarget == LogTargetGCP && cfg.Parent == "" {
		return cfg, fmt.Errorf("parent (e.g. projects/PROJECT_ID) is required for GCP target and could not be resolved from env vars or metadata: %w", ErrProjectIDMissing)
	}

	return cfg, nil
}

// parseLogTargetEnv converts a string from the environment into a LogTarget enum.
// It handles case-insensitive matching and falls back to the provided default
// if the string is empty or unrecognized.
func parseLogTargetEnv(valStr string, defaultVal LogTarget) LogTarget {
	switch strings.ToLower(strings.TrimSpace(valStr)) {
	case "gcp":
		return LogTargetGCP
	case "stdout":
		return LogTargetStdout
	case "stderr":
		return LogTargetStderr
	case "file":
		return LogTargetFile
	case "": // Empty string means use default
		return defaultVal
	default:
		fmt.Fprintf(os.Stderr, "[slogcp config] WARNING: Invalid log target value %q in env var, defaulting to %v\n", valStr, defaultVal)
		return defaultVal
	}
}

// parseLevelEnv converts a level string from the environment into a slog.Level.
// It handles standard slog levels, extended GCP levels, and numeric values.
// Falls back to the provided default if the string is empty or invalid.
func parseLevelEnv(levelStr string, defaultLvl slog.Level) slog.Level {
	trimmedLevelStr := strings.ToLower(strings.TrimSpace(levelStr))
	if trimmedLevelStr == "" {
		return defaultLvl
	}

	switch trimmedLevelStr {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "default":
		return internalLevelDefault
	case "notice":
		return internalLevelNotice
	case "critical":
		return internalLevelCritical
	case "alert":
		return internalLevelAlert
	case "emergency":
		return internalLevelEmergency
	default:
		if levelVal, err := strconv.Atoi(trimmedLevelStr); err == nil {
			return slog.Level(levelVal)
		}
		fmt.Fprintf(os.Stderr, "[slogcp config] WARNING: Invalid log level value %q in env var, defaulting to %v\n", levelStr, defaultLvl)
		return defaultLvl
	}
}

// parseBoolEnv converts a boolean string from the environment into a bool.
// It understands various representations (true/false, yes/no, 1/0, on/off).
// Falls back to the provided default if the string is empty or unrecognized.
func parseBoolEnv(boolStr string, defaultVal bool) bool {
	trimmed := strings.ToLower(strings.TrimSpace(boolStr))
	if trimmed == "" {
		return defaultVal
	}
	switch trimmed {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		fmt.Fprintf(os.Stderr, "[slogcp config] WARNING: Invalid boolean value %q in env var, defaulting to %v\n", boolStr, defaultVal)
		return defaultVal
	}
}

// parseIntPtrEnv converts an integer string from the environment into an *int.
// Returns nil if the string is empty or invalid, which allows distinguishing
// between unset values and explicit zeros in the configuration.
func parseIntPtrEnv(valStr string) *int {
	trimmed := strings.TrimSpace(valStr)
	if trimmed == "" {
		return nil
	}
	if i, err := strconv.Atoi(trimmed); err == nil {
		return &i
	}
	fmt.Fprintf(os.Stderr, "[slogcp config] WARNING: Invalid integer value %q in env var, ignoring\n", valStr)
	return nil
}

// parseDurationPtrEnvMS converts a millisecond value from the environment into a *time.Duration.
// Returns nil if the string is empty or invalid, or if the parsed value is negative.
// This is used for timeout and threshold settings that require a duration.
func parseDurationPtrEnvMS(valStr string) *time.Duration {
	trimmed := strings.TrimSpace(valStr)
	if trimmed == "" {
		return nil
	}
	if ms, err := strconv.Atoi(trimmed); err == nil && ms >= 0 {
		d := time.Duration(ms) * time.Millisecond
		return &d
	}
	fmt.Fprintf(os.Stderr, "[slogcp config] WARNING: Invalid non-negative millisecond value %q in env var, ignoring\n", valStr)
	return nil
}

// parseBoolPtrEnv converts a boolean string from the environment into a *bool.
// Returns nil if the string is empty or invalid, which allows distinguishing
// between unset values and explicit false values in the configuration.
func parseBoolPtrEnv(valStr string) *bool {
	trimmed := strings.ToLower(strings.TrimSpace(valStr))
	if trimmed == "" {
		return nil
	}
	switch trimmed {
	case "true", "1", "yes", "on":
		b := true
		return &b
	case "false", "0", "no", "off":
		b := false
		return &b
	default:
		fmt.Fprintf(os.Stderr, "[slogcp config] WARNING: Invalid boolean value %q in env var, ignoring\n", valStr)
		return nil
	}
}

// Max length is *less than* 512 chars (i.e., 0..511)
// See: https://cloud.google.com/logging/docs/reference/v2/rest/v2/LogEntry
const logIDMaxLen = 512

// normalizeLogID returns a cleaned LOG_ID or an error if it violates
// Cloud Logging constraints. It does not URL-encode or build logName.
func normalizeLogID(s string) (string, error) {
	// Trim spaces, strip surrounding quotes, then trim again.
	s = strings.TrimSpace(s)

	for len(s) > 0 && (s[0] == '"' || s[0] == '\'') {
		s = s[1:]
	}
	for n := len(s); n > 0 && (s[n-1] == '"' || s[n-1] == '\''); n = len(s) {
		s = s[:n-1]
	}

	s = strings.TrimSpace(s)

	// Use default when empty.
	if s == "" {
		s = slogcpDefaultGCPLogID
	}

	if len(s) >= logIDMaxLen {
		return "", fmt.Errorf("LOG_ID must be < %d characters", logIDMaxLen)
	}

	// Validate allowed ASCII set.
	for i := 0; i < len(s); i++ {
		b := s[i]
		if ('a' <= b && b <= 'z') ||
			('A' <= b && b <= 'Z') ||
			('0' <= b && b <= '9') ||
			b == '/' || b == '_' || b == '-' || b == '.' {
			continue
		}
		return "", fmt.Errorf(
			"LOG_ID contains invalid character %q; allowed are letters, digits, '/', '_', '-', '.'",
			b,
		)
	}

	return s, nil
}

// NormalizeLogID exposes normalizeLogID for use by other packages.
func NormalizeLogID(s string) (string, error) {
	return normalizeLogID(s)
}
