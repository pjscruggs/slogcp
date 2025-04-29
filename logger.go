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
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/pjscruggs/slogcp/internal/gcp"
)

// Logger wraps slog.Logger to provide GCP-specific logging features.
//
// It integrates with the Cloud Logging API for structured JSON logging,
// automatically extracts trace context from context.Context when available,
// optionally includes source code location, allows dynamic level control,
// and correlates logs with Cloud Trace and Error Reporting.
//
// Use the standard slog methods like DebugContext, InfoContext, WarnContext, ErrorContext,
// Log, and LogAttrs. For severity levels specific to GCP (Default, Notice, Critical,
// Alert, Emergency), use the dedicated convenience methods provided by this logger,
// such as NoticeContext, CriticalContext, AlertContext, etc., or their AttrsContext variants.
// These methods mirror the standard slog convenience methods.
//
// When used with context.Context values containing OpenTelemetry spans,
// the logger automatically includes trace and span IDs in log entries.
//
// Use the http.Middleware function from the slogcp/http subpackage for HTTP services
// or the grpc interceptors from the slogcp/grpc subpackage for gRPC services
// to automatically correlate logs with request traces.
//
// Call Close or Flush during application shutdown to ensure buffered logs are sent.
type Logger struct {
	*slog.Logger                    // Embed slog.Logger for standard methods.
	clientMgr    *gcp.ClientManager // Manages the underlying Cloud Logging client.
	levelVar     *slog.LevelVar     // Controls the dynamic logging level.
	projectID    string             // Stores the configured GCP Project ID.
	closeOnce    sync.Once          // Ensures Close actions run only once.
}

// New creates a new GCP-aware slog.Logger configured for Cloud Logging.
//
// It determines the GCP Project ID using a defined precedence:
// 1. Value provided via `WithProjectID` option.
// 2. `GOOGLE_CLOUD_PROJECT` environment variable.
// 3. Automatic detection via the metadata server (if running on GCP).
// If no Project ID can be determined, initialization fails.
//
// It automatically detects Cloud Run metadata if available via the underlying client.
// It initializes the Cloud Logging client needed for sending logs.
//
// Call Close on the returned logger during application shutdown to flush
// buffered logs and release resources.
//
// Example:
//
//	// Ensure GOOGLE_CLOUD_PROJECT is set or running on GCP
//	logger, err := slogcp.New(slogcp.WithLevel(slog.LevelDebug))
//	if err != nil {
//	    log.Fatalf("Failed to create logger: %v", err)
//	}
//	defer logger.Close()
//	// Use standard slog methods
//	logger.InfoContext(context.Background(), "Informational message")
//	// Use slogcp convenience methods for GCP levels
//	logger.NoticeContext(context.Background(), "Application started", "version", "1.0.1")
func New(opts ...Option) (*Logger, error) {
	// Process functional options to populate the internal options struct.
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}

	// Load configuration from environment variables.
	// This also attempts Project ID detection from env/metadata.
	cfg, err := gcp.LoadConfig()
	if err != nil {
		// Log fatal config errors directly to stderr as logger isn't ready.
		fmt.Fprintf(os.Stderr, "[slogcp] FATAL: Failed to load configuration: %v\n", err)
		return nil, fmt.Errorf("failed to load configuration: %w", err)
	}

	// Apply programmatic overrides from functional options to the loaded config.
	// Options take precedence over environment variables.
	if o.level != nil {
		cfg.InitialLevel = *o.level
	}
	if o.addSource != nil {
		cfg.AddSource = *o.addSource
	}
	if o.stackTraceEnabled != nil {
		cfg.StackTraceEnabled = *o.stackTraceEnabled
	}
	if o.stackTraceLevel != nil {
		cfg.StackTraceLevel = *o.stackTraceLevel
	}
	if o.projectID != nil {
		cfg.ProjectID = *o.projectID // Override detected/env project ID if provided.
	}
	// Final check for Project ID after potential override.
	if cfg.ProjectID == "" {
		err = fmt.Errorf("GCP Project ID is missing: provide via WithProjectID option, GOOGLE_CLOUD_PROJECT env var, or run on GCP infrastructure: %w", gcp.ErrProjectIDMissing)
		fmt.Fprintf(os.Stderr, "[slogcp] FATAL: %v\n", err)
		return nil, err
	}

	// Create a dynamic level controller based on the final configuration.
	levelVar := new(slog.LevelVar)
	levelVar.Set(cfg.InitialLevel)

	// Prepare options for the underlying Cloud Logging client's logger instance.
	clientOpts := gcp.ClientOptions{
		EntryCountThreshold: o.entryCountThreshold,
		DelayThreshold:      o.delayThreshold,
		MonitoredResource:   o.monitoredResource, // Pass explicit resource if provided.
	}

	// Create and initialize the internal client manager.
	// This handles the creation and lifecycle of the actual Cloud Logging client.
	clientMgr := gcp.NewClientManager(cfg, UserAgent, clientOpts, levelVar)
	if err := clientMgr.Initialize(); err != nil {
		// Initialization error is already logged to stderr by ClientManager.
		return nil, fmt.Errorf("failed to initialize cloud logging client: %w", err)
	}

	// Get the initialized logger interface from the manager.
	// This interface is used by the handler to send log entries.
	loggerAPI, err := clientMgr.GetLogger()
	if err != nil {
		// This indicates an internal issue if Initialize succeeded but GetLogger failed.
		_ = clientMgr.Close() // Attempt cleanup.
		fmt.Fprintf(os.Stderr, "[slogcp] FATAL: Failed to get logger instance after initialization: %v\n", err)
		return nil, fmt.Errorf("failed to get logger instance after initialization: %w", err)
	}

	// Create the custom slog handler that formats logs for GCP and uses the loggerAPI.
	handler := gcp.NewGcpHandler(cfg, loggerAPI, levelVar)

	// Create the base slog.Logger using the custom handler.
	baseLogger := slog.New(handler)

	// Apply initial attributes provided via functional options.
	if len(o.initialAttrs) > 0 {
		// Convert []slog.Attr to []any for slog.Logger.With
		kvPairs := make([]any, 0, len(o.initialAttrs)*2)
		for _, attr := range o.initialAttrs {
			kvPairs = append(kvPairs, attr.Key, attr.Value.Any())
		}
		// Reassign baseLogger to the new logger with attributes.
		baseLogger = baseLogger.With(kvPairs...)
	}

	// Apply initial group provided via functional options.
	if o.initialGroup != "" {
		// Reassign baseLogger to the new logger with the group.
		baseLogger = baseLogger.WithGroup(o.initialGroup)
	}

	// Return the wrapped Logger containing the configured slog.Logger,
	// the client manager, and the determined project ID.
	return &Logger{
		Logger:    baseLogger,
		clientMgr: clientMgr,
		levelVar:  levelVar,
		projectID: cfg.ProjectID, // Store the final project ID.
	}, nil
}

// Close flushes any buffered log entries and releases resources associated
// with the underlying Cloud Logging client.
//
// It is safe to call multiple times. It's recommended to defer this call
// in your main function or handle it during graceful shutdown.
// Returns the first significant error encountered during flush or close,
// or an error if the client was not properly initialized.
func (l *Logger) Close() error {
	var err error
	l.closeOnce.Do(func() {
		// Log shutdown initiation to stderr for visibility.
		fmt.Fprintf(os.Stderr, "[slogcp] INFO: Initiating logger shutdown...\n")
		// Delegate the close operation to the internal client manager.
		err = l.clientMgr.Close()
		if err == nil {
			fmt.Fprintf(os.Stderr, "[slogcp] INFO: Logger shutdown complete.\n")
		} else {
			// Log shutdown failure to stderr.
			fmt.Fprintf(os.Stderr, "[slogcp] ERROR: Logger shutdown failed: %v\n", err)
		}
	})
	return err
}

// Flush forces any buffered log entries to be sent to Cloud Logging immediately.
//
// This is useful during graceful shutdown or before a critical operation.
// It's automatically called by Close, but can be called independently.
// Returns an error if flushing fails (e.g., client not initialized or flush error).
func (l *Logger) Flush() error {
	// Retrieve the logger interface from the client manager.
	loggerAPI, err := l.clientMgr.GetLogger()
	if err != nil {
		// Log failure to get logger to stderr.
		fmt.Fprintf(os.Stderr, "[slogcp] ERROR: Failed to get logger for flush: %v\n", err)
		return fmt.Errorf("failed to get logger for flush: %w", err)
	}
	// Delegate the flush operation to the logger interface.
	err = loggerAPI.Flush()
	if err != nil {
		// Log flush failure to stderr.
		fmt.Fprintf(os.Stderr, "[slogcp] ERROR: Failed to flush logs: %v\n", err)
		return err // Return the underlying flush error.
	}
	return nil
}

// SetLevel dynamically changes the minimum logging level for this logger instance
// and any loggers derived from it via With or WithGroup.
func (l *Logger) SetLevel(level slog.Level) {
	l.levelVar.Set(level)
}

// GetLevel returns the current minimum logging level configured for this logger.
func (l *Logger) GetLevel() slog.Level {
	return l.levelVar.Level()
}

// ProjectID returns the Google Cloud Project ID associated with this logger instance.
// This ID is used internally for formatting trace IDs and may be useful for
// other integrations.
func (l *Logger) ProjectID() string {
	return l.projectID
}

// DefaultContext logs at the LevelDefault severity with the given context.
// It accepts a message string and a list of key-value pairs.
func (l *Logger) DefaultContext(ctx context.Context, msg string, args ...any) {
	// Call the embedded logger's Log method, converting the slogcp.Level
	// constant to the required slog.Level type using its Level() method.
	l.Logger.Log(ctx, LevelDefault.Level(), msg, args...)
}

// NoticeContext logs at the LevelNotice severity with the given context.
// It accepts a message string and a list of key-value pairs.
func (l *Logger) NoticeContext(ctx context.Context, msg string, args ...any) {
	l.Logger.Log(ctx, LevelNotice.Level(), msg, args...)
}

// CriticalContext logs at the LevelCritical severity with the given context.
// It accepts a message string and a list of key-value pairs.
func (l *Logger) CriticalContext(ctx context.Context, msg string, args ...any) {
	l.Logger.Log(ctx, LevelCritical.Level(), msg, args...)
}

// AlertContext logs at the LevelAlert severity with the given context.
// It accepts a message string and a list of key-value pairs.
func (l *Logger) AlertContext(ctx context.Context, msg string, args ...any) {
	l.Logger.Log(ctx, LevelAlert.Level(), msg, args...)
}

// EmergencyContext logs at the LevelEmergency severity with the given context.
// It accepts a message string and a list of key-value pairs.
func (l *Logger) EmergencyContext(ctx context.Context, msg string, args ...any) {
	l.Logger.Log(ctx, LevelEmergency.Level(), msg, args...)
}

// DefaultAttrsContext logs at the LevelDefault severity with the given context and attributes.
// It accepts a message string and a list of slog.Attr values.
func (l *Logger) DefaultAttrsContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	// Call the embedded logger's LogAttrs method, converting the slogcp.Level
	// constant to the required slog.Level type using its Level() method.
	l.Logger.LogAttrs(ctx, LevelDefault.Level(), msg, attrs...)
}

// NoticeAttrsContext logs at the LevelNotice severity with the given context and attributes.
// It accepts a message string and a list of slog.Attr values.
func (l *Logger) NoticeAttrsContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.Logger.LogAttrs(ctx, LevelNotice.Level(), msg, attrs...)
}

// CriticalAttrsContext logs at the LevelCritical severity with the given context and attributes.
// It accepts a message string and a list of slog.Attr values.
func (l *Logger) CriticalAttrsContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.Logger.LogAttrs(ctx, LevelCritical.Level(), msg, attrs...)
}

// AlertAttrsContext logs at the LevelAlert severity with the given context and attributes.
// It accepts a message string and a list of slog.Attr values.
func (l *Logger) AlertAttrsContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.Logger.LogAttrs(ctx, LevelAlert.Level(), msg, attrs...)
}

// EmergencyAttrsContext logs at the LevelEmergency severity with the given context and attributes.
// It accepts a message string and a list of slog.Attr values.
func (l *Logger) EmergencyAttrsContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.Logger.LogAttrs(ctx, LevelEmergency.Level(), msg, attrs...)
}
