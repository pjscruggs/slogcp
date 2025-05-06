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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/pjscruggs/slogcp/internal/gcp"
)

// clientManagerInterface defines the methods required from the underlying
// client manager used by the Logger. This allows for mocking in tests.
type clientManagerInterface interface {
	Initialize() error
	Close() error
	GetLogger() (gcp.GcpLoggerAPI, error) // Returns interface for logging/flushing
	GetLeveler() slog.Leveler
}

// newClientManagerFunc holds the function used to create the client manager.
// Tests can override this variable to inject mocks.
var newClientManagerFunc = func(cfg gcp.Config, ua string, co gcp.ClientOptions, lv *slog.LevelVar) clientManagerInterface {
	// The concrete *gcp.ClientManager returned by gcp.NewClientManager
	// implicitly satisfies the clientManagerInterface.
	return gcp.NewClientManager(cfg, ua, co, lv)
}

// Logger wraps slog.Logger to provide GCP-specific logging features or
// fallback structured JSON logging to standard output/error.
//
// When configured for GCP (default):
// It integrates with the Cloud Logging API for structured JSON logging,
// automatically extracts trace context from context.Context when available,
// optionally includes source code location, allows dynamic level control,
// and correlates logs with Cloud Trace and Error Reporting.
// Call Close or Flush during application shutdown to ensure buffered logs are sent.
//
// When configured for Stdout/Stderr (via options or environment variable):
// It logs structured JSON to the specified output stream. It respects level
// and source location settings. Trace context is not automatically extracted
// or formatted for GCP correlation. Severity is represented as a string field.
// Close and Flush operations are no-ops in this mode.
//
// Use the standard slog methods like DebugContext, InfoContext, WarnContext, ErrorContext,
// Log, and LogAttrs. For severity levels specific to GCP (Default, Notice, Critical,
// Alert, Emergency), use the dedicated convenience methods provided by this logger,
// such as NoticeContext, CriticalContext, AlertContext, etc., or their AttrsContext variants.
// These methods mirror the standard slog convenience methods and work in both modes.
type Logger struct {
	*slog.Logger
	clientMgr clientManagerInterface // Interface to the client manager (nil in fallback mode).
	levelVar  *slog.LevelVar         // Controls the dynamic logging level.
	projectID string                 // Stores the configured GCP Project ID (may be empty in fallback mode).
	closeOnce sync.Once              // Ensures Close actions run only once.
}

// New creates a new slog.Logger configured for Cloud Logging (default) or
// structured JSON output to stdout/stderr.
//
// Configuration precedence for log target:
// 1. Value provided via `WithLogTarget` option.
// 2. `SLOGCP_LOG_TARGET` environment variable ("gcp", "stdout", "stderr").
// 3. Default: GCP Cloud Logging.
//
// Configuration precedence for Project ID (only required for GCP target):
// 1. Value provided via `WithProjectID` option.
// 2. `GOOGLE_CLOUD_PROJECT` environment variable.
// 3. Automatic detection via the metadata server (if running on GCP).
// If the target is GCP and no Project ID can be determined, initialization fails.
//
// If the target is GCP and client initialization fails, the logger will
// automatically fall back to logging structured JSON to standard output,
// printing a warning to standard error about the failure.
//
// Call Close on the returned logger during application shutdown to flush
// buffered logs and release resources *if* operating in GCP mode. Close is a
// no-op in stdout/stderr mode.
//
// Example (GCP default):
//
//	// Ensure GOOGLE_CLOUD_PROJECT is set or running on GCP
//	logger, err := slogcp.New(slogcp.WithLevel(slogcp.LevelDebug))
//	if err != nil {
//	    log.Fatalf("Failed to create logger: %v", err)
//	}
//	defer logger.Close() // Important for GCP mode
//	logger.InfoContext(context.Background(), "GCP log")
//
// Example (Force stdout):
//
//	logger, err := slogcp.New(slogcp.WithLogTarget(slogcp.LogTargetStdout))
//	if err != nil {
//	    log.Fatalf("Failed to create logger: %v", err) // Should not fail for stdout
//	}
//	// No defer logger.Close() needed for stdout/stderr
//	logger.NoticeContext(context.Background(), "Stdout log", "version", "1.0.1")
func New(opts ...Option) (*Logger, error) {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}

	cfg, err := gcp.LoadConfig()
	if err != nil && !errors.Is(err, gcp.ErrProjectIDMissing) {
		fmt.Fprintf(os.Stderr, "[slogcp] FATAL: Failed to load configuration: %v\n", err)
		return nil, fmt.Errorf("failed to load configuration: %w", err)
	}

	finalLogTarget := cfg.LogTarget
	if o.logTarget != nil {
		finalLogTarget = *o.logTarget
	}
	cfg.LogTarget = finalLogTarget

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
		cfg.ProjectID = *o.projectID
	}
	if o.replaceAttr != nil {
		cfg.ReplaceAttrFunc = o.replaceAttr
	}

	if finalLogTarget == LogTargetGCP && cfg.ProjectID == "" {
		err = fmt.Errorf("GCP Project ID is missing: provide via WithProjectID option, GOOGLE_CLOUD_PROJECT env var, or run on GCP infrastructure (required for GCP target): %w", gcp.ErrProjectIDMissing)
		fmt.Fprintf(os.Stderr, "[slogcp] FATAL: %v\n", err)
		return nil, err
	}

	levelVar := new(slog.LevelVar)
	levelVar.Set(cfg.InitialLevel)

	var clientMgr clientManagerInterface // Use the interface type
	var handler slog.Handler

	if finalLogTarget == LogTargetStdout || finalLogTarget == LogTargetStderr {
		var writer io.Writer = os.Stdout
		if finalLogTarget == LogTargetStderr {
			writer = os.Stderr
		}
		fmt.Fprintf(os.Stderr, "[slogcp] INFO: Logging configured for target: %s\n", cfg.LogTarget.String())

		handlerOpts := slog.HandlerOptions{
			AddSource: cfg.AddSource,
			Level:     levelVar,
		}

		// If custom replaceAttr is provided, chain it with our standard attribute replacer
		if o.replaceAttr != nil {
			originalReplace := o.replaceAttr
			handlerOpts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
				// First apply custom replacer
				a = originalReplace(groups, a)
				// Then apply the standard slogcp severity mapping
				return replaceAttrForFallback(groups, a)
			}
		} else {
			handlerOpts.ReplaceAttr = replaceAttrForFallback
		}

		handler = slog.NewJSONHandler(writer, &handlerOpts)
		// clientMgr remains nil

	} else { // LogTargetGCP
		clientOpts := gcp.ClientOptions{
			EntryCountThreshold: o.entryCountThreshold,
			DelayThreshold:      o.delayThreshold,
			MonitoredResource:   o.monitoredResource,
		}

		// Use the factory function to create the client manager instance.
		clientMgr = newClientManagerFunc(cfg, UserAgent, clientOpts, levelVar)
		initErr := clientMgr.Initialize()

		if initErr != nil {
			fmt.Fprintf(os.Stderr, "[slogcp] WARNING: Failed to initialize Cloud Logging client: %v. Falling back to structured logging on stdout.\n", initErr)

			handlerOpts := slog.HandlerOptions{
				AddSource: cfg.AddSource,
				Level:     levelVar,
			}

			// If custom replaceAttr is provided, chain it with our standard attribute replacer
			if o.replaceAttr != nil {
				originalReplace := o.replaceAttr
				handlerOpts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
					// First apply custom replacer
					a = originalReplace(groups, a)
					// Then apply the standard slogcp severity mapping
					return replaceAttrForFallback(groups, a)
				}
			} else {
				handlerOpts.ReplaceAttr = replaceAttrForFallback
			}

			handler = slog.NewJSONHandler(os.Stdout, &handlerOpts)
			clientMgr = nil // Ensure clientMgr is nil in fallback mode

		} else {
			// Get the logger API (which handles Log and Flush) from the manager.
			loggerAPI, err := clientMgr.GetLogger()
			if err != nil {
				// This indicates an internal issue if Initialize succeeded but GetLogger failed.
				_ = clientMgr.Close() // Attempt cleanup.
				fmt.Fprintf(os.Stderr, "[slogcp] FATAL: Failed to get logger instance after initialization: %v\n", err)
				return nil, fmt.Errorf("failed to get logger instance after initialization: %w", err)
			}
			// Get the leveler from the manager.
			leveler := clientMgr.GetLeveler()
			if leveler == nil { // Should not happen if init succeeded, but check defensively.
				_ = clientMgr.Close()
				fmt.Fprintf(os.Stderr, "[slogcp] FATAL: Failed to get leveler after initialization\n")
				return nil, fmt.Errorf("failed to get leveler after initialization")
			}
			// Create the GCP handler using the logger API and leveler.
			handler = gcp.NewGcpHandler(cfg, loggerAPI, leveler)
		}
	}

	// Apply middleware functions in order (if any)
	if len(o.middlewares) > 0 {
		for _, middleware := range o.middlewares {
			handler = middleware(handler)
		}
	}

	// Create the base logger with the final handler (potentially transformed by middleware)
	baseLogger := slog.New(handler)

	// Apply initial attributes provided via functional options.
	if len(o.initialAttrs) > 0 {
		kvPairs := make([]any, 0, len(o.initialAttrs)*2)
		for _, attr := range o.initialAttrs {
			kvPairs = append(kvPairs, attr.Key, attr.Value.Any())
		}
		baseLogger = baseLogger.With(kvPairs...)
	}

	// Apply initial group provided via functional options.
	if o.initialGroup != "" {
		baseLogger = baseLogger.WithGroup(o.initialGroup)
	}

	return &Logger{
		Logger:    baseLogger,
		clientMgr: clientMgr, // Store the interface (nil in fallback/stdout/stderr)
		levelVar:  levelVar,
		projectID: cfg.ProjectID,
	}, nil
}

// replaceAttrForFallback is a slog.ReplaceAttr function used by the fallback
// JSON handler (stdout/stderr). It replaces the standard level key/value
// with a "severity" key and the string representation of the slogcp.Level.
func replaceAttrForFallback(groups []string, a slog.Attr) slog.Attr {
	if a.Key == slog.LevelKey && len(groups) == 0 {
		level, ok := a.Value.Any().(slog.Level)
		if ok {
			levelStr := Level(level).String()
			return slog.String("severity", levelStr)
		}
	}
	return a
}

// Close flushes any buffered log entries and releases resources associated
// with the underlying Cloud Logging client *if* the logger is operating in
// GCP mode (i.e., not stdout/stderr and initialization succeeded).
// It is a no-op if the logger is writing to stdout/stderr or in fallback mode.
// It is safe to call multiple times.
func (l *Logger) Close() error {
	if l.clientMgr == nil {
		return nil // No-op if no client manager (stdout/stderr/fallback)
	}

	var err error
	l.closeOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "[slogcp] INFO: Initiating logger shutdown (GCP client)...\n")
		// Delegate the close operation to the client manager interface.
		err = l.clientMgr.Close()
		if err == nil {
			fmt.Fprintf(os.Stderr, "[slogcp] INFO: Logger shutdown complete (GCP client).\n")
		} else {
			fmt.Fprintf(os.Stderr, "[slogcp] ERROR: Logger shutdown failed (GCP client): %v\n", err)
		}
	})
	return err
}

// Flush forces any buffered log entries to be sent to Cloud Logging immediately
// *if* the logger is operating in GCP mode.
// It is a no-op if the logger is writing to stdout/stderr or in fallback mode.
// Returns an error if flushing fails.
func (l *Logger) Flush() error {
	if l.clientMgr == nil {
		return nil // No-op if no client manager
	}

	// Retrieve the logger interface (which has the Flush method) from the client manager.
	loggerAPI, err := l.clientMgr.GetLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[slogcp] ERROR: Failed to get logger for flush: %v\n", err)
		return fmt.Errorf("failed to get logger for flush: %w", err)
	}

	// Delegate the flush operation to the logger interface.
	err = loggerAPI.Flush()
	if err != nil {
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

// ProjectID returns the Google Cloud Project ID associated with this logger instance,
// if configured. Returns an empty string if not configured for GCP.
func (l *Logger) ProjectID() string {
	return l.projectID
}

// DefaultContext logs at the LevelDefault severity with the given context.
func (l *Logger) DefaultContext(ctx context.Context, msg string, args ...any) {
	l.Log(ctx, LevelDefault.Level(), msg, args...)
}

// NoticeContext logs at the LevelNotice severity with the given context.
func (l *Logger) NoticeContext(ctx context.Context, msg string, args ...any) {
	l.Log(ctx, LevelNotice.Level(), msg, args...)
}

// CriticalContext logs at the LevelCritical severity with the given context.
func (l *Logger) CriticalContext(ctx context.Context, msg string, args ...any) {
	l.Log(ctx, LevelCritical.Level(), msg, args...)
}

// AlertContext logs at the LevelAlert severity with the given context.
func (l *Logger) AlertContext(ctx context.Context, msg string, args ...any) {
	l.Log(ctx, LevelAlert.Level(), msg, args...)
}

// EmergencyContext logs at the LevelEmergency severity with the given context.
func (l *Logger) EmergencyContext(ctx context.Context, msg string, args ...any) {
	l.Log(ctx, LevelEmergency.Level(), msg, args...)
}

// DefaultAttrsContext logs at the LevelDefault severity with the given context and attributes.
func (l *Logger) DefaultAttrsContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.LogAttrs(ctx, LevelDefault.Level(), msg, attrs...)
}

// NoticeAttrsContext logs at the LevelNotice severity with the given context and attributes.
func (l *Logger) NoticeAttrsContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.LogAttrs(ctx, LevelNotice.Level(), msg, attrs...)
}

// CriticalAttrsContext logs at the LevelCritical severity with the given context and attributes.
func (l *Logger) CriticalAttrsContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.LogAttrs(ctx, LevelCritical.Level(), msg, attrs...)
}

// AlertAttrsContext logs at the LevelAlert severity with the given context and attributes.
func (l *Logger) AlertAttrsContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.LogAttrs(ctx, LevelAlert.Level(), msg, attrs...)
}

// EmergencyAttrsContext logs at the LevelEmergency severity with the given context and attributes.
func (l *Logger) EmergencyAttrsContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.LogAttrs(ctx, LevelEmergency.Level(), msg, attrs...)
}
