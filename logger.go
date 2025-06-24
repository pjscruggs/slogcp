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
	"strings"
	"sync"

	"github.com/pjscruggs/slogcp/internal/gcp"
)

// LabelsGroup is the special slog.Group name used to specify dynamic labels
// that should be added to the Cloud Logging labels field rather than the
// jsonPayload. Attributes within this group are automatically converted to
// strings and merged with any common labels configured via WithGCPCommonLabel.
//
// Example:
//
//	logger.Info("user action",
//	    slog.Group(slogcp.LabelsGroup,
//	        slog.String("user_id", "u123"),
//	        slog.String("action", "login"),
//	        slog.Bool("success", true),  // Converted to "true"
//	    ),
//	)
const LabelsGroup = "logging.googleapis.com/labels"

// Logger wraps slog.Logger to provide GCP-specific logging features or
// fallback structured JSON logging to standard output/error/file.
//
// It integrates with the Cloud Logging API (default) or writes structured JSON
// locally based on configuration. It handles trace correlation, source location,
// GCP severity levels, and optional stack traces.
//
// Use standard slog methods (InfoContext, ErrorContext, etc.) and the additional
// GCP severity level methods (NoticeContext, CriticalContext, etc.).
// Call Close() during shutdown to flush buffers and release resources (especially
// important in GCP API mode or when logging to a file opened by slogcp).
//
// For file-based logging, Logger supports integration with external log rotation
// tools (like logrotate) via the ReopenLogFile method, and correctly handles
// user-provided io.Writer instances that might perform their own rotation
// (like lumberjack).
type Logger struct {
	*slog.Logger
	clientMgr      ClientManagerInterface // Manages GCP client lifecycle
	levelVar       *slog.LevelVar         // Controls dynamic level filtering
	resolvedCfg    gcp.Config             // Final resolved configuration
	closeOnce      sync.Once              // Ensures Close() is called only once
	inFallbackMode bool                   // Indicates logger is operating in automatic fallback mode

	mu              sync.Mutex // Protects ownedFileHandle
	ownedFileHandle *os.File   // The *os.File if slogcp opened it via path
}

// IsInFallbackMode returns true if the logger was initialized in fallback mode.
//
// In fallback mode, structured JSON logs are written to stdout instead of the
// Google Cloud Logging API. This occurs when all of these conditions are met:
//  1. No explicit logging target was configured (via options or environment variables)
//  2. The default GCP logging target failed to initialize
//  3. Automatic fallback was successful
//
// Applications can check this method to detect fallback mode and adjust behavior
// or notify users accordingly.
func (l *Logger) IsInFallbackMode() bool {
	return l.inFallbackMode
}

// wasTargetExplicitlyConfigured determines if the user explicitly specified a logging
// target through options or environment variables.
//
// Returns true if any of these were set:
//   - WithLogTarget() option
//   - WithRedirectToFile() option
//   - WithRedirectWriter() option
//   - SLOGCP_LOG_TARGET environment variable
//   - SLOGCP_REDIRECT_AS_JSON_TARGET environment variable
//
// This helps distinguish between using the default target (which can fall back)
// and the user explicitly choosing a target (which should not fall back).
func wasTargetExplicitlyConfigured(builderState *options, envLogTargetStr, envRedirectTargetStr string) bool {
	// Check programmatic options
	if builderState.logTarget != nil ||
		builderState.redirectFilePath != nil ||
		builderState.redirectWriter != nil {
		return true
	}

	// Check environment variables
	if envLogTargetStr != "" || envRedirectTargetStr != "" {
		return true
	}

	return false
}

// resolveParentAndProjectID determines the final GCP Parent and ProjectID values
// based on programmatic options, environment variables, and metadata server.
//
// It handles different scenarios:
//   - Parent specified explicitly (projects/X, folders/Y, etc.)
//   - ProjectID specified explicitly
//   - Both specified (checks for consistency if both are project-based)
//   - Neither specified (checks if required for GCP target)
//
// It modifies cfg.Parent and cfg.ProjectID in place and allows the initialization
// process to continue even when a Parent is missing for GCP target, enabling
// automatic fallback to local logging.
func resolveParentAndProjectID(cfg *gcp.Config, builderState *options, baseConfigLoadErr error) error {
	// Apply programmatic Parent setting if provided
	if builderState.parent != nil {
		cfg.Parent = *builderState.parent
		// If parent is project-based, extract ProjectID
		if strings.HasPrefix(*builderState.parent, "projects/") {
			cfg.ProjectID = strings.TrimPrefix(*builderState.parent, "projects/")
		} else if builderState.projectID != nil {
			// For non-project parent, use explicit ProjectID if provided
			cfg.ProjectID = *builderState.projectID
		}
	} else if builderState.projectID != nil {
		// Apply programmatic ProjectID if provided
		cfg.ProjectID = *builderState.projectID
		// Update Parent only if it wasn't set or was project-based
		if cfg.Parent == "" || strings.HasPrefix(cfg.Parent, "projects/") {
			cfg.Parent = "projects/" + *builderState.projectID
		}
	}

	// If Parent is project-based, ensure ProjectID is consistent
	if cfg.Parent != "" && strings.HasPrefix(cfg.Parent, "projects/") {
		derivedProjectID := strings.TrimPrefix(cfg.Parent, "projects/")
		if cfg.ProjectID == "" {
			cfg.ProjectID = derivedProjectID
		} else if cfg.ProjectID != derivedProjectID {
			fmt.Fprintf(os.Stderr, "[slogcp] WARNING: Mismatch between GCP Parent (%s) and ProjectID (%s). Using Parent for client initialization and ProjectID for trace formatting.\n",
				cfg.Parent, cfg.ProjectID)
		}
	}

	// Don't return an error for missing parent - this allows fallback to happen
	// The initialization will fail later with appropriate error, triggering fallback
	return nil
}

// applyProgrammaticOptions merges programmatic configuration options into the
// provided config. It handles target resolution, writer setup, credentials,
// and all behavioral options.
//
// It modifies the provided config in place and returns an error if conflicting
// options are detected or required values cannot be resolved.
func applyProgrammaticOptions(cfg *gcp.Config, builderState *options, baseConfigLoadErr error) error {
	// Process log target
	if builderState.logTarget != nil {
		cfg.LogTarget = *builderState.logTarget
	}

	// Handle file path option (takes precedence over direct writer if both were somehow set,
	// though option functions should prevent this)
	if builderState.redirectFilePath != nil {
		if builderState.redirectWriter != nil {
			fmt.Fprintf(os.Stderr, "[slogcp] WARNING: Both WithRedirectToFile and WithRedirectWriter used; file path %q takes precedence\n",
				*builderState.redirectFilePath)
		}
		cfg.LogTarget = gcp.LogTargetFile
		cfg.OpenedFilePath = *builderState.redirectFilePath
		cfg.RedirectWriter = nil     // Mark that slogcp needs to open it
		cfg.ClosableUserWriter = nil // No user writer involved
	} else if builderState.redirectWriter != nil {

		// WithRedirectWriter was used
		cfg.RedirectWriter = builderState.redirectWriter // User's writer

		// Only store in ClosableUserWriter if it's NOT os.Stdout or os.Stderr
		if c, ok := builderState.redirectWriter.(io.Closer); ok {
			// Don't store standard output/error as closable writers
			if builderState.redirectWriter != os.Stdout &&
				builderState.redirectWriter != os.Stderr {
				cfg.ClosableUserWriter = c
			}
		}
		cfg.OpenedFilePath = "" // Mark that slogcp does not own the file path

		// Infer target if not explicitly set by WithLogTarget
		if builderState.logTarget == nil {
			switch builderState.redirectWriter {
			case os.Stdout:
				cfg.LogTarget = gcp.LogTargetStdout
			case os.Stderr:
				cfg.LogTarget = gcp.LogTargetStderr
			}
			// If it's a custom writer, LogTarget might remain default (GCP) or be set by user.
			// Conflicts are checked later.
		}
	}

	// Set default writers for standard targets if not explicitly provided by user
	// and not path-based (which handles its own writer creation).
	if cfg.OpenedFilePath == "" { // Only if not path-based
		if cfg.LogTarget == gcp.LogTargetStdout && cfg.RedirectWriter == nil {
			cfg.RedirectWriter = os.Stdout
		}
		if cfg.LogTarget == gcp.LogTargetStderr && cfg.RedirectWriter == nil {
			cfg.RedirectWriter = os.Stderr
		}
	}

	// Resolve parent and project ID
	if err := resolveParentAndProjectID(cfg, builderState, baseConfigLoadErr); err != nil {
		return err
	}

	// Apply behavior options
	if builderState.level != nil {
		cfg.InitialLevel = *builderState.level
	}
	if builderState.addSource != nil {
		cfg.AddSource = *builderState.addSource
	}
	if builderState.stackTraceEnabled != nil {
		cfg.StackTraceEnabled = *builderState.stackTraceEnabled
	}
	if builderState.stackTraceLevel != nil {
		cfg.StackTraceLevel = *builderState.stackTraceLevel
	}
	if builderState.replaceAttr != nil {
		cfg.ReplaceAttrFunc = builderState.replaceAttr
	}

	// Process middlewares
	if len(builderState.middlewares) > 0 {
		cfg.Middlewares = make([]func(slog.Handler) slog.Handler, len(builderState.middlewares))
		for i, mw := range builderState.middlewares {
			current := mw
			cfg.Middlewares[i] = func(h slog.Handler) slog.Handler {
				return current(h)
			}
		}
	}

	// Apply GCP client options
	if builderState.clientScopes != nil {
		cfg.ClientScopes = builderState.clientScopes
	}
	if builderState.clientOnErrorFunc != nil {
		cfg.ClientOnErrorFunc = builderState.clientOnErrorFunc
	}

	// Merge common labels
	cfg.GCPCommonLabels = mergeMaps(cfg.GCPCommonLabels, builderState.programmaticCommonLabels)

	// Apply other GCP-specific options
	if builderState.gcpMonitoredResource != nil {
		cfg.GCPMonitoredResource = builderState.gcpMonitoredResource
	}
	if builderState.gcpConcurrentWriteLimit != nil {
		cfg.GCPConcurrentWriteLimit = builderState.gcpConcurrentWriteLimit
	}
	if builderState.gcpDelayThreshold != nil {
		cfg.GCPDelayThreshold = builderState.gcpDelayThreshold
	}
	if builderState.gcpEntryCountThreshold != nil {
		cfg.GCPEntryCountThreshold = builderState.gcpEntryCountThreshold
	}
	if builderState.gcpEntryByteThreshold != nil {
		cfg.GCPEntryByteThreshold = builderState.gcpEntryByteThreshold
	}
	if builderState.gcpEntryByteLimit != nil {
		cfg.GCPEntryByteLimit = builderState.gcpEntryByteLimit
	}
	if builderState.gcpBufferedByteLimit != nil {
		cfg.GCPBufferedByteLimit = builderState.gcpBufferedByteLimit
	}
	if builderState.gcpContextFunc != nil {
		cfg.GCPContextFunc = builderState.gcpContextFunc
	}
	if builderState.gcpDefaultContextTimeout != nil {
		cfg.GCPDefaultContextTimeout = *builderState.gcpDefaultContextTimeout
	}
	if builderState.gcpPartialSuccess != nil {
		cfg.GCPPartialSuccess = builderState.gcpPartialSuccess
	}

	// Apply initial attrs and group
	if len(builderState.programmaticAttrs) > 0 {
		cfg.InitialAttrs = append([]slog.Attr{}, builderState.programmaticAttrs...)
	}
	if builderState.programmaticGroup != nil {
		cfg.InitialGroup = *builderState.programmaticGroup
	}

	// Final validation
	if cfg.LogTarget == gcp.LogTargetGCP && builderState.redirectWriter != nil {
		return errors.New("conflicting options: LogTarget is GCP but WithRedirectWriter was also used")
	}
	if cfg.LogTarget == gcp.LogTargetFile && cfg.RedirectWriter == nil && cfg.OpenedFilePath == "" {
		return errors.New("log target LogTargetFile requires a file path via WithRedirectToFile or SLOGCP_REDIRECT_AS_JSON_TARGET, or a writer via WithRedirectWriter")
	}

	return nil
}

// sourceAwareHandler is a transparent wrapper that advertises
// HasSource()==true to the slog.Logger constructor.  When slog detects this
// method, it populates Record.PC automatically in newRecord, eliminating the
// need for manual stack inspection.
type sourceAwareHandler struct{ slog.Handler }

// HasSource reports that sourceAwareHandler wants the slog runtime to capture
// caller information.
//
// When this method returns true, slog populates each Record with the program
// counter of the logging call before invoking the handler. That allows
// handlers downstream—such as the GCP emitter—to resolve accurate file, line,
// and function metadata without performing their own stack inspection.
func (h sourceAwareHandler) HasSource() bool { return true }

// initializeLogger creates a Logger instance with the provided configuration.
//
// It sets up the appropriate handler based on the log target (GCP or redirect),
// applies middlewares in reverse order (last middleware wraps innermost handler),
// and constructs the final Logger instance.
//
// Returns the configured Logger or an error if initialization fails.
func initializeLogger(cfg *gcp.Config, levelV *slog.LevelVar, userAgent string) (*Logger, error) {
	levelV.Set(cfg.InitialLevel)

	var cMgr ClientManagerInterface
	var slogHandler slog.Handler
	var ownedFileHandleForLogger *os.File

	// Prepare cfg.RedirectWriter for the handler based on LogTarget and OpenedFilePath
	if cfg.LogTarget == gcp.LogTargetFile && cfg.OpenedFilePath != "" {
		// slogcp is responsible for opening this file.
		f, err := os.OpenFile(cfg.OpenedFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, fmt.Errorf("slogcp: failed to open log file %q: %w", cfg.OpenedFilePath, err)
		}
		// The handler will use a SwitchableWriter pointing to this file.
		// This modifies the cfg instance that will be stored in resolvedCfg.
		cfg.RedirectWriter = gcp.NewSwitchableWriter(f)
		ownedFileHandleForLogger = f
	} else if cfg.LogTarget == gcp.LogTargetStdout && cfg.RedirectWriter == nil {
		cfg.RedirectWriter = os.Stdout
	} else if cfg.LogTarget == gcp.LogTargetStderr && cfg.RedirectWriter == nil {
		cfg.RedirectWriter = os.Stderr
	}
	// If cfg.RedirectWriter was already set by user (via WithRedirectWriter), it's used as is.
	// If cfg.LogTarget is GCP, cfg.RedirectWriter remains nil.

	if cfg.LogTarget == gcp.LogTargetGCP {
		concreteMgr := gcp.NewClientManager(*cfg, userAgent, levelV)
		if err := concreteMgr.Initialize(); err != nil {
			_ = concreteMgr.Close()
			return nil, fmt.Errorf("failed to initialize GCP client manager: %w", err)
		}
		gcpAPILogger, err := concreteMgr.GetLogger()
		if err != nil {
			_ = concreteMgr.Close()
			return nil, fmt.Errorf("failed to get GCP logger after initialization: %w", err)
		}
		slogHandler = gcp.NewGcpHandler(*cfg, gcpAPILogger, levelV)
		cMgr = concreteMgr
	} else {
		// Redirect mode (stdout/stderr/file/custom writer)
		if cfg.RedirectWriter == nil { // Should be caught by applyProgrammaticOptions or earlier logic
			return nil, fmt.Errorf("internal error: log target %s selected but no redirect writer available in config", cfg.LogTarget)
		}
		slogHandler = gcp.NewGcpHandler(*cfg, nil, levelV) // Pass nil for gcpAPILogger
	}

	// Apply middlewares in reverse order
	baseHandler := slogHandler
	for i := len(cfg.Middlewares) - 1; i >= 0; i-- {
		if mw := cfg.Middlewares[i]; mw != nil {
			baseHandler = mw(baseHandler)
		}
	}

	// Inject PC‑capturing wrapper when the option is enabled.
	if cfg.AddSource {
		baseHandler = sourceAwareHandler{Handler: baseHandler}
	}

	slogCoreLogger := slog.New(baseHandler)
	l := &Logger{
		Logger:          slogCoreLogger,
		clientMgr:       cMgr,
		levelVar:        levelV,
		resolvedCfg:     *cfg, // Store the fully resolved config
		ownedFileHandle: ownedFileHandleForLogger,
	}
	return l, nil
}

// New creates a new Logger instance with the specified options. It initializes
// logging according to a three-tier configuration strategy:
//
//  1. Default base settings are applied first
//  2. Environment variables override defaults
//  3. Programmatic options override environment variables
//
// Default Behavior and Fallback:
// If no logging target is explicitly specified (via options or environment
// variables), New attempts to use the Google Cloud Logging API as the default
// target. If this default GCP initialization fails (e.g., due to missing
// credentials in local development), New will automatically fall back to
// structured JSON logging to stdout.
//
// Applications can detect this fallback using the IsInFallbackMode() method.
//
// When a target IS explicitly configured (e.g., WithLogTarget(LogTargetGCP)),
// fallback does not occur, and initialization errors are returned to the caller.
//
// The returned Logger should have its Close() method called during application
// shutdown to ensure all logs are flushed and resources are released properly.
func New(opts ...Option) (*Logger, error) {
	baseCfg, baseConfigLoadErr := gcp.LoadConfig()
	if baseConfigLoadErr != nil && !errors.Is(baseConfigLoadErr, gcp.ErrProjectIDMissing) {
		return nil, baseConfigLoadErr
	}

	builderState := &options{}
	for _, opt := range opts {
		if opt != nil {
			opt(builderState)
		}
	}

	envLogTargetStr := os.Getenv(gcp.EnvLogTarget)
	envRedirectTargetStr := os.Getenv(gcp.EnvRedirectAsJSONTarget)
	explicitTargetConfigured := wasTargetExplicitlyConfigured(builderState, envLogTargetStr, envRedirectTargetStr)

	currentCfg := baseCfg
	if err := applyProgrammaticOptions(&currentCfg, builderState, baseConfigLoadErr); err != nil {
		return nil, err
	}

	levelV := new(slog.LevelVar)
	logger, initErr := initializeLogger(&currentCfg, levelV, UserAgent)

	if initErr != nil && !explicitTargetConfigured && currentCfg.LogTarget == gcp.LogTargetGCP {
		fmt.Fprintf(os.Stderr, "[slogcp] WARNING: Failed to initialize GCP logging: %v\n", initErr)
		fmt.Fprintf(os.Stderr, "[slogcp] INFO: Falling back to structured JSON logging on stdout\n")

		stdoutFallbackCfg := gcp.Config{
			LogTarget:         gcp.LogTargetStdout,
			RedirectWriter:    os.Stdout, // Explicitly stdout for fallback
			InitialLevel:      currentCfg.InitialLevel,
			AddSource:         currentCfg.AddSource,
			StackTraceEnabled: currentCfg.StackTraceEnabled,
			StackTraceLevel:   currentCfg.StackTraceLevel,
			ReplaceAttrFunc:   currentCfg.ReplaceAttrFunc,
			Middlewares:       currentCfg.Middlewares,
			InitialAttrs:      currentCfg.InitialAttrs,
			InitialGroup:      currentCfg.InitialGroup,
			ProjectID:         currentCfg.ProjectID,
			// OpenedFilePath and ClosableUserWriter should be empty/nil for stdout fallback
		}

		fallbackLogger, fallbackErr := initializeLogger(&stdoutFallbackCfg, levelV, UserAgent)
		if fallbackErr != nil {
			return nil, fmt.Errorf("fallback to stdout logging failed: %w (original GCP init error: %v)", fallbackErr, initErr)
		}
		fallbackLogger.inFallbackMode = true
		return fallbackLogger, nil
	}

	return logger, initErr
}

// Close flushes buffered logs and releases resources associated with the logger.
//
// Behavior varies based on configuration:
//   - For GCP mode, it flushes logs and closes the Cloud Logging client.
//   - If slogcp opened a file for logging (e.g., via WithRedirectToFile),
//     it closes that file.
//   - If a user-provided io.Writer that implements io.Closer was given
//     (e.g., via WithRedirectWriter, such as a lumberjack.Logger),
//     its Close method is called.
//
// This method is safe to call multiple times; subsequent calls are no-ops
// and return the error from the first call, if any. Applications should
// call Close during shutdown to ensure all logs are processed and resources
// are released.
func (l *Logger) Close() error {
	var firstErr error
	l.closeOnce.Do(func() {
		if l.clientMgr != nil {
			err := l.clientMgr.Close()
			if err != nil {
				firstErr = err
				fmt.Fprintf(os.Stderr, "[slogcp] ERROR closing GCP client manager: %v\n", err)
			}
		}

		l.mu.Lock()
		if l.ownedFileHandle != nil {
			err := l.ownedFileHandle.Close()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				fmt.Fprintf(os.Stderr, "[slogcp] ERROR closing owned log file: %v\n", err)
			}
			l.ownedFileHandle = nil
		}
		l.mu.Unlock()

		if l.resolvedCfg.ClosableUserWriter != nil {
			err := l.resolvedCfg.ClosableUserWriter.Close()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				fmt.Fprintf(os.Stderr, "[slogcp] ERROR closing user-provided redirect writer: %v\n", err)
			}
		}

		// If slogcp opened the file, resolvedCfg.RedirectWriter is a *gcp.SwitchableWriter.
		// Close it to make its state consistent (sets internal writer to nil).
		// The actual *os.File (ownedFileHandle) is already handled above.
		if l.resolvedCfg.OpenedFilePath != "" && l.resolvedCfg.RedirectWriter != nil {
			if sw, ok := l.resolvedCfg.RedirectWriter.(io.Closer); ok {
				if err := sw.Close(); err != nil {
					fmt.Fprintf(os.Stderr, "[slogcp] INFO: Error closing switchable writer wrapper: %v\n", err)
				}
			}
		}
	})
	return firstErr
}

// ReopenLogFile closes and reopens the log file if it was originally configured
// by slogcp using a file path (e.g., via WithRedirectToFile or the
// SLOGCP_REDIRECT_AS_JSON_TARGET environment variable with a "file:" prefix).
// This method is intended for integration with external log rotation tools
// like logrotate, which typically rename the active log file and then signal
// the application to reopen its log file descriptor on the original path.
//
// This method is a no-op and returns nil if the logger was configured:
//   - With a user-provided io.Writer (e.g., via WithRedirectWriter, such as
//     a lumberjack.Logger, which manages its own file rotation).
//   - To log to destinations other than a file managed by slogcp (e.g.,
//     GCP, stdout, stderr).
//
// In such cases, the user-provided writer or the logging system itself is
// responsible for file management.
//
// It returns an error if reopening the file fails. If logging is critical
// and reopening fails, the application may need to handle this by, for example,
// re-initializing the logger or terminating.
func (l *Logger) ReopenLogFile() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.resolvedCfg.OpenedFilePath == "" || l.resolvedCfg.LogTarget != gcp.LogTargetFile {
		// Not configured to open a file by path, or not a file target.
		return nil
	}

	if l.ownedFileHandle != nil {
		if err := l.ownedFileHandle.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "[slogcp] WARNING: Error closing current log file during reopen: %v\n", err)
		}
		l.ownedFileHandle = nil
	}

	newFileHandle, err := os.OpenFile(l.resolvedCfg.OpenedFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[slogcp] ERROR: Failed to reopen log file %q: %v. Logging may be disrupted.\n", l.resolvedCfg.OpenedFilePath, err)
		return fmt.Errorf("slogcp: failed to reopen log file %q: %w", l.resolvedCfg.OpenedFilePath, err)
	}
	l.ownedFileHandle = newFileHandle

	if sw, ok := l.resolvedCfg.RedirectWriter.(*gcp.SwitchableWriter); ok {
		sw.SetWriter(newFileHandle)
		fmt.Fprintf(os.Stderr, "[slogcp] INFO: Log file %q reopened successfully.\n", l.resolvedCfg.OpenedFilePath)
	} else {
		fmt.Fprintf(os.Stderr, "[slogcp] ERROR: Could not update handler's writer during reopen for %q. Logging may be misdirected.\n", l.resolvedCfg.OpenedFilePath)
		_ = newFileHandle.Close()
		l.ownedFileHandle = nil
		return fmt.Errorf("slogcp: handler's writer is not switchable for reopen on path %s", l.resolvedCfg.OpenedFilePath)
	}

	return nil
}

// Flush forces any buffered log entries to be sent immediately.
// In GCP mode, it calls Flush on the underlying client manager.
// In redirect modes, it attempts to flush the underlying writer if the
// writer implements a Flush() error method.
//
// Returns an error if flushing fails or if the logger has no flushable resources.
func (l *Logger) Flush() error {
	if l.clientMgr != nil {
		return l.clientMgr.Flush()
	}

	var writerToFlush io.Writer
	if l.resolvedCfg.RedirectWriter != nil {
		if sw, ok := l.resolvedCfg.RedirectWriter.(*gcp.SwitchableWriter); ok {
			writerToFlush = sw.GetCurrentWriter()
		} else {
			writerToFlush = l.resolvedCfg.RedirectWriter
		}
	}

	if flusher, ok := writerToFlush.(interface{ Flush() error }); ok {
		return flusher.Flush()
	}

	return nil
}

// ProjectID returns the resolved Google Cloud Project ID used by the logger.
// May be empty if not configured or not applicable (e.g., redirect mode).
func (l *Logger) ProjectID() string { return l.resolvedCfg.ProjectID }

// Parent returns the resolved Google Cloud resource parent string used by the logger
// (e.g., "projects/my-proj", "folders/123"). May be empty if not configured.
func (l *Logger) Parent() string { return l.resolvedCfg.Parent }

// SetLevel dynamically changes the minimum logging level.
// Messages below this level will be discarded.
func (l *Logger) SetLevel(level slog.Level) { l.levelVar.Set(level) }

// GetLevel returns the current minimum logging level.
func (l *Logger) GetLevel() slog.Level { return l.levelVar.Level() }

// DefaultContext logs at LevelDefault severity (below Debug).
// This corresponds to the GCP DEFAULT severity level.
func (l *Logger) DefaultContext(ctx context.Context, msg string, args ...any) {
	l.Log(ctx, LevelDefault.Level(), msg, args...)
}

// NoticeContext logs at LevelNotice severity (between Info and Warn).
// This corresponds to the GCP NOTICE severity level.
func (l *Logger) NoticeContext(ctx context.Context, msg string, args ...any) {
	l.Log(ctx, LevelNotice.Level(), msg, args...)
}

// CriticalContext logs at LevelCritical severity (above Error).
// This corresponds to the GCP CRITICAL severity level.
func (l *Logger) CriticalContext(ctx context.Context, msg string, args ...any) {
	l.Log(ctx, LevelCritical.Level(), msg, args...)
}

// AlertContext logs at LevelAlert severity (above Critical).
// This corresponds to the GCP ALERT severity level.
func (l *Logger) AlertContext(ctx context.Context, msg string, args ...any) {
	l.Log(ctx, LevelAlert.Level(), msg, args...)
}

// EmergencyContext logs at LevelEmergency severity (highest level).
// This corresponds to the GCP EMERGENCY severity level.
func (l *Logger) EmergencyContext(ctx context.Context, msg string, args ...any) {
	l.Log(ctx, LevelEmergency.Level(), msg, args...)
}

// DefaultAttrsContext logs at LevelDefault severity with attributes.
// This corresponds to the GCP DEFAULT severity level.
func (l *Logger) DefaultAttrsContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.LogAttrs(ctx, LevelDefault.Level(), msg, attrs...)
}

// NoticeAttrsContext logs at LevelNotice severity with attributes.
// This corresponds to the GCP NOTICE severity level.
func (l *Logger) NoticeAttrsContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.LogAttrs(ctx, LevelNotice.Level(), msg, attrs...)
}

// CriticalAttrsContext logs at LevelCritical severity with attributes.
// This corresponds to the GCP CRITICAL severity level.
func (l *Logger) CriticalAttrsContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.LogAttrs(ctx, LevelCritical.Level(), msg, attrs...)
}

// AlertAttrsContext logs at LevelAlert severity with attributes.
// This corresponds to the GCP ALERT severity level.
func (l *Logger) AlertAttrsContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.LogAttrs(ctx, LevelAlert.Level(), msg, attrs...)
}

// EmergencyAttrsContext logs at LevelEmergency severity with attributes.
// This corresponds to the GCP EMERGENCY severity level.
func (l *Logger) EmergencyAttrsContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.LogAttrs(ctx, LevelEmergency.Level(), msg, attrs...)
}

// mergeMaps combines two string maps, with overlay values taking precedence over
// base values for the same keys. Returns nil if both inputs are empty or nil.
// This preserves memory efficiency by avoiding empty map allocations.
func mergeMaps(base, overlay map[string]string) map[string]string {
	if len(overlay) == 0 {
		if len(base) == 0 { // Both empty or nil
			return nil
		}
		return base // Return original base (might be nil if base was empty)
	}

	sizeHint := len(overlay)
	if base != nil {
		sizeHint += len(base)
	}
	merged := make(map[string]string, sizeHint)

	for k, v := range base {
		merged[k] = v
	}
	for k, v := range overlay {
		merged[k] = v
	}

	if len(merged) == 0 { // Should only happen if both base and overlay were effectively empty
		return nil
	}
	return merged
}
