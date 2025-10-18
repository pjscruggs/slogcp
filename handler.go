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

// Package slogcp provides a "batteries included" structured logging solution
// tailored for Google Cloud Platform (GCP). It provides a [slog.Handler] that
// integrates seamlessly with the standard Go [log/slog] package and offers
// enhanced features for cloud environments.
//
// The core of the package is the [Handler] type, which can be configured to
// send structured JSON logs directly to the Cloud Logging API or to local
// destinations like stdout, stderr, or a file.
package slogcp

import (
	"maps"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pjscruggs/slogcp/internal/gcp"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
)

// LabelsGroup is the special slog.Group name used to specify dynamic labels
// that should be added to the Cloud Logging "labels" field rather than the
// main "jsonPayload".
const LabelsGroup = "logging.googleapis.com/labels"

// Handler is a [slog.Handler] that writes structured logs optimized for
// Google Cloud Platform or to a local destination as JSON. It manages the
// GCP client lifecycle and must be closed via its Close method to ensure logs
// are flushed before application termination.
//
// A Handler is safe for concurrent use by multiple goroutines.
type Handler struct {
	slog.Handler // Embed the core handler.

	clientMgr      ClientManagerInterface // Manages GCP client lifecycle.
	resolvedCfg    gcp.Config             // Final resolved configuration.
	inFallbackMode bool                   // Indicates if the handler is in automatic fallback mode.
	closeOnce      sync.Once
	internalLogger *slog.Logger

	mu              sync.Mutex // Protects ownedFileHandle
	ownedFileHandle *os.File   // The *os.File if slogcp opened it via path
}

// NewHandler creates a [slog.Handler] configured by programmatic [Option] functions
// and environment variables. This is the main entry point for the library.
//
// The defaultWriter is used if the configuration resolves to a local target
// (like stdout, stderr, or file) but no specific writer is provided through
// options or environment variables. It is ignored if the target is GCP.
//
// It is crucial to call the returned handler's Close method on application
// shutdown to flush buffered logs and release resources.
//
// Example for local development:
//
//	handler, err := slogcp.NewHandler(os.Stdout,
//		slogcp.WithLevel(slog.LevelDebug),
//		slogcp.WithSourceLocationEnabled(true),
//	)
//	if err != nil {
//		log.Fatalf("failed to create handler: %v", err)
//	}
//	defer handler.Close()
//
//	logger := slog.New(handler)
//	logger.Debug("This is a debug message.")
func NewHandler(defaultWriter io.Writer, opts ...Option) (*Handler, error) {
	builderState := &options{}
	for _, opt := range opts {
		if opt != nil {
			opt(builderState)
		}
	}

	// Set up the internal logger for the library's own diagnostics.
	// Default to a disabled logger if none is provided by the user.
	internalLogger := builderState.internalLogger
	if internalLogger == nil {
		internalLogger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	baseCfg, baseConfigLoadErr := gcp.LoadConfig(internalLogger)
	if baseConfigLoadErr != nil && !errors.Is(baseConfigLoadErr, gcp.ErrProjectIDMissing) {
		return nil, baseConfigLoadErr
	}

	envLogTargetStr := os.Getenv(gcp.EnvLogTarget)
	envRedirectTargetStr := os.Getenv(gcp.EnvRedirectAsJSONTarget)
	explicitTargetConfigured := wasTargetExplicitlyConfigured(builderState, envLogTargetStr, envRedirectTargetStr)

	currentCfg := baseCfg
	if err := applyProgrammaticOptions(&currentCfg, builderState, internalLogger); err != nil {
		return nil, err
	}

	// If no writer was configured for a local target, use the provided default.
	if currentCfg.LogTarget != gcp.LogTargetGCP && currentCfg.RedirectWriter == nil && currentCfg.OpenedFilePath == "" {
		currentCfg.RedirectWriter = defaultWriter
	}

	levelV := new(slog.LevelVar)
	handler, initErr := initializeHandler(&currentCfg, levelV, UserAgent, internalLogger)

	// Fallback logic: if GCP was the target and initialization failed,
	// and no explicit redirect was configured, fall back to a local writer.
	if initErr != nil && currentCfg.LogTarget == gcp.LogTargetGCP && !explicitTargetConfigured {
		var fallbackWriter io.Writer = os.Stdout
		if builderState.fallbackWriter != nil {
			fallbackWriter = builderState.fallbackWriter
		}

		fallbackDest := "the configured fallback writer"
		if f, ok := fallbackWriter.(*os.File); ok {
			if f.Name() == os.Stdout.Name() {
				fallbackDest = "stdout"
			} else if f.Name() == os.Stderr.Name() {
				fallbackDest = "stderr"
			}
		}

		internalLogger.Warn("Failed to initialize GCP logging, falling back to local writer", "error", initErr, "destination", fallbackDest)

		fallbackCfg, _ := gcp.LoadConfig(internalLogger)
		if err := applyProgrammaticOptions(&fallbackCfg, builderState, internalLogger); err != nil {
			return nil, fmt.Errorf("failed to re-apply options for fallback: %w", err)
		}

		fallbackCfg.RedirectWriter = fallbackWriter
		fallbackCfg.OpenedFilePath = ""
		fallbackCfg.ClosableUserWriter = nil

		if c, ok := fallbackWriter.(io.Closer); ok {
			if f, isFile := fallbackWriter.(*os.File); !isStdStream(f, isFile) {
				fallbackCfg.ClosableUserWriter = c
			}
		}

		switch f := fallbackWriter.(type) {
		case *os.File:
			if f.Name() == os.Stderr.Name() {
				fallbackCfg.LogTarget = gcp.LogTargetStderr
			} else {
				fallbackCfg.LogTarget = gcp.LogTargetStdout
			}
		default:
			fallbackCfg.LogTarget = gcp.LogTargetStdout
		}

		fallbackHandler, fallbackErr := initializeHandler(&fallbackCfg, levelV, UserAgent, internalLogger)
		if fallbackErr != nil {
			return nil, fmt.Errorf("fallback logging failed to initialize: %w (original GCP init error: %v)", fallbackErr, initErr)
		}
		fallbackHandler.inFallbackMode = true
		return fallbackHandler, nil
	}

	return handler, initErr
}

func isStdStream(f *os.File, ok bool) bool {
	if !ok || f == nil {
		return false
	}
	name := f.Name()
	return name == os.Stdout.Name() || name == os.Stderr.Name()
}

// initializeHandler creates a Handler instance with the provided configuration.
func initializeHandler(cfg *gcp.Config, levelV *slog.LevelVar, userAgent string, internalLogger *slog.Logger) (*Handler, error) {
	levelV.Set(cfg.InitialLevel)

	var cMgr ClientManagerInterface
	var slogCoreHandler slog.Handler
	var ownedFileHandleForLogger *os.File

	runtimeInfo := gcp.DetectRuntimeInfo()

	if cfg.LogTarget == gcp.LogTargetFile && cfg.OpenedFilePath != "" {
		f, err := os.OpenFile(cfg.OpenedFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, fmt.Errorf("slogcp: failed to open log file %q: %w", cfg.OpenedFilePath, err)
		}
		cfg.RedirectWriter = gcp.NewSwitchableWriter(f)
		ownedFileHandleForLogger = f
	}

	if cfg.LogTarget == gcp.LogTargetGCP {
		concreteMgr := gcp.NewClientManager(*cfg, userAgent, levelV, internalLogger)
		if err := concreteMgr.Initialize(); err != nil {
			_ = concreteMgr.Close()
			return nil, fmt.Errorf("failed to initialize GCP client manager: %w", err)
		}
		gcpAPILogger, err := concreteMgr.GetLogger()
		if err != nil {
			_ = concreteMgr.Close()
			return nil, fmt.Errorf("failed to get GCP logger after initialization: %w", err)
		}
		slogCoreHandler = gcp.NewGcpHandler(*cfg, gcpAPILogger, levelV, internalLogger, runtimeInfo)
		cMgr = concreteMgr
	} else {
		if cfg.RedirectWriter == nil {
			return nil, fmt.Errorf("internal error: log target %s selected but no writer available", cfg.LogTarget)
		}
		slogCoreHandler = gcp.NewGcpHandler(*cfg, nil, levelV, internalLogger, runtimeInfo)
	}

	for i := len(cfg.Middlewares) - 1; i >= 0; i-- {
		if mw := cfg.Middlewares[i]; mw != nil {
			slogCoreHandler = mw(slogCoreHandler)
		}
	}

	if cfg.AddSource {
		slogCoreHandler = sourceAwareHandler{Handler: slogCoreHandler}
	}

	h := &Handler{
		Handler:         slogCoreHandler,
		clientMgr:       cMgr,
		resolvedCfg:     *cfg,
		ownedFileHandle: ownedFileHandleForLogger,
		internalLogger:  internalLogger,
	}
	return h, nil
}

// Close flushes any buffered logs and releases resources. For GCP mode, it closes
// the Cloud Logging client. It also closes any user-provided [io.Writer] that
// implements [io.Closer] or any file opened by slogcp.
//
// Close is safe to call concurrently and repeatedly; only the first call will
// perform the shutdown. It returns the first error encountered during the
// process. It is typically called via defer immediately after the handler is
// created.
//
//	handler, err := slogcp.NewHandler(os.Stdout, nil)
//	if err != nil {
//		log.Fatalf("failed to create handler: %v", err)
//	}
//	defer handler.Close()
func (h *Handler) Close() error {
	var firstErr error
	h.closeOnce.Do(func() {
		if h.clientMgr != nil {
			if err := h.clientMgr.Close(); err != nil {
				firstErr = err
				h.internalLogger.Error("Error closing GCP client manager", "error", err)
			}
		}

		h.mu.Lock()
		if h.ownedFileHandle != nil {
			if err := h.ownedFileHandle.Close(); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				h.internalLogger.Error("Error closing owned log file", "error", err)
			}
			h.ownedFileHandle = nil
		}
		h.mu.Unlock()

		if h.resolvedCfg.ClosableUserWriter != nil {
			if err := h.resolvedCfg.ClosableUserWriter.Close(); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				h.internalLogger.Error("Error closing user-provided writer", "error", err)
			}
		}
	})
	return firstErr
}

// ReopenLogFile closes and reopens the log file if it was configured by path
// using [WithRedirectToFile] or the SLOGCP_REDIRECT_AS_JSON_TARGET environment
// variable. This is useful for integration with external log rotation tools
// like logrotate.
//
// It is a no-op if the handler is not configured to log to a file via path.
func (h *Handler) ReopenLogFile() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.resolvedCfg.OpenedFilePath == "" || h.resolvedCfg.LogTarget != gcp.LogTargetFile {
		return nil
	}

	if h.ownedFileHandle != nil {
		if err := h.ownedFileHandle.Close(); err != nil {
			h.internalLogger.Warn("Error closing current log file during reopen", "error", err)
		}
	}

	newFile, err := os.OpenFile(h.resolvedCfg.OpenedFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("slogcp: failed to reopen log file %q: %w", h.resolvedCfg.OpenedFilePath, err)
	}
	h.ownedFileHandle = newFile

	if sw, ok := h.resolvedCfg.RedirectWriter.(*gcp.SwitchableWriter); ok {
		sw.SetWriter(newFile)
	} else {
		_ = newFile.Close()
		h.ownedFileHandle = nil
		return fmt.Errorf("slogcp: handler's writer is not switchable for reopen")
	}

	return nil
}

// IsInFallbackMode reports whether the handler failed to initialize its primary
// target (GCP) and fell back to logging to a local writer.
func (h *Handler) IsInFallbackMode() bool {
	return h.inFallbackMode
}

// --- Package-Level Helper Functions ---

// DefaultContext logs at [LevelDefault] using the provided standard [slog.Logger].
func DefaultContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	logger.Log(ctx, LevelDefault.Level(), msg, args...)
}

// NoticeContext logs at [LevelNotice] using the provided standard [slog.Logger].
//
//	logger := slog.New(mySlogCpHandler)
//	slogcp.NoticeContext(context.Background(), logger, "User signed up", "user_id", "u-123")
func NoticeContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	logger.Log(ctx, LevelNotice.Level(), msg, args...)
}

// CriticalContext logs at [LevelCritical] using the provided standard [slog.Logger].
func CriticalContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	logger.Log(ctx, LevelCritical.Level(), msg, args...)
}

// AlertContext logs at [LevelAlert] using the provided standard [slog.Logger].
func AlertContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	logger.Log(ctx, LevelAlert.Level(), msg, args...)
}

// EmergencyContext logs at [LevelEmergency] using the provided standard [slog.Logger].
func EmergencyContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	logger.Log(ctx, LevelEmergency.Level(), msg, args...)
}

// --- Option Types and Functions ---

// Option configures a Handler during initialization via the NewHandler function.
type Option func(*options)

// Middleware transforms the handler chain.
type Middleware func(slog.Handler) slog.Handler

// LogTarget defines available destinations for log output. It mirrors the
// internal gcp.LogTarget enum so callers do not need to import internal types.
type LogTarget = gcp.LogTarget

const (
	LogTargetGCP    LogTarget = gcp.LogTargetGCP
	LogTargetStdout LogTarget = gcp.LogTargetStdout
	LogTargetStderr LogTarget = gcp.LogTargetStderr
	LogTargetFile   LogTarget = gcp.LogTargetFile
)

// options holds the builder state for programmatic configuration.
// It is not exported and is only used internally by NewHandler.
type options struct {
	level             *slog.Level
	addSource         *bool
	stackTraceEnabled *bool
	stackTraceLevel   *slog.Level
	logTarget         *LogTarget
	redirectWriter    io.Writer
	fallbackWriter    io.Writer
	redirectFilePath  *string
	replaceAttr       func([]string, slog.Attr) slog.Attr
	middlewares       []Middleware
	programmaticAttrs []slog.Attr
	programmaticGroup *string
	internalLogger    *slog.Logger

	projectID                *string
	parent                   *string
	traceProjectID           *string
	gcpLogID                 *string
	clientScopes             []string
	clientOnErrorFunc        func(error)
	programmaticCommonLabels map[string]string
	gcpMonitoredResource     *mrpb.MonitoredResource
	gcpConcurrentWriteLimit  *int
	gcpDelayThreshold        *time.Duration
	gcpEntryCountThreshold   *int
	gcpEntryByteThreshold    *int
	gcpEntryByteLimit        *int
	gcpBufferedByteLimit     *int
	gcpContextFunc           func() (context.Context, func())
	gcpDefaultContextTimeout *time.Duration
	gcpPartialSuccess        *bool
}

// WithInternalLogger provides a logger for slogcp's own internal diagnostics,
// such as warnings about configuration or fallback notifications. If not provided,
// internal logging is disabled.
func WithInternalLogger(logger *slog.Logger) Option {
	return func(o *options) { o.internalLogger = logger }
}

// WithLevel returns an Option that sets the minimum logging level.
func WithLevel(level slog.Level) Option { return func(o *options) { o.level = &level } }

// WithSourceLocationEnabled returns an Option that enables or disables
// including source code location (file, line, function) in log entries.
func WithSourceLocationEnabled(enabled bool) Option {
	return func(o *options) { o.addSource = &enabled }
}

// WithStackTraceEnabled returns an Option that enables or disables automatic
// stack trace capture for logs at or above the configured stack trace level.
func WithStackTraceEnabled(enabled bool) Option {
	return func(o *options) { o.stackTraceEnabled = &enabled }
}

// WithStackTraceLevel returns an Option that sets the minimum level for stack traces.
func WithStackTraceLevel(level slog.Level) Option {
	return func(o *options) { o.stackTraceLevel = &level }
}

// WithLogTarget returns an Option that explicitly sets the logging destination.
func WithLogTarget(target LogTarget) Option { return func(o *options) { o.logTarget = &target } }

// WithReplaceAttr returns an Option that sets a function to replace or modify
// attributes before they are logged.
func WithReplaceAttr(fn func([]string, slog.Attr) slog.Attr) Option {
	return func(o *options) { o.replaceAttr = fn }
}

// WithMiddleware registers a middleware that decorates the Handler.
func WithMiddleware(mw Middleware) Option {
	return func(o *options) {
		if mw != nil {
			o.middlewares = append(o.middlewares, mw)
		}
	}
}

// WithAttrs returns an Option that adds attributes to be included with every log record.
func WithAttrs(attrs []slog.Attr) Option {
	return func(o *options) {
		if len(attrs) == 0 {
			return
		}
		attrsCopy := make([]slog.Attr, len(attrs))
		copy(attrsCopy, attrs)
		o.programmaticAttrs = append(o.programmaticAttrs, attrsCopy...)
	}
}

// WithGroup returns an Option that sets an initial group namespace for the logger.
func WithGroup(name string) Option { return func(o *options) { o.programmaticGroup = &name } }

// WithRedirectToStdout configures logging to standard output.
func WithRedirectToStdout() Option {
	return func(o *options) {
		target := gcp.LogTargetStdout
		o.logTarget = &target
		o.redirectWriter = os.Stdout
		o.redirectFilePath = nil
	}
}

// WithRedirectToStderr configures logging to standard error.
func WithRedirectToStderr() Option {
	return func(o *options) {
		target := gcp.LogTargetStderr
		o.logTarget = &target
		o.redirectWriter = os.Stderr
		o.redirectFilePath = nil
	}
}

// WithRedirectToFile configures logging to a specified file path. The file
// will be created if it does not exist and logs will be appended.
//
// Example:
//
//	handler, err := slogcp.NewHandler(nil,
//		slogcp.WithRedirectToFile("/var/log/myapp.log"),
//	)
func WithRedirectToFile(filePath string) Option {
	return func(o *options) {
		target := gcp.LogTargetFile
		o.logTarget = &target
		o.redirectFilePath = &filePath
		o.redirectWriter = nil
	}
}

// WithRedirectWriter configures logging to a custom [io.Writer]. The caller
// is responsible for the lifecycle of the writer, but [Handler.Close] will
// call the writer's Close method if it implements [io.Closer].
// This is useful for integrating with self-rotating log writers.
func WithRedirectWriter(writer io.Writer) Option {
	return func(o *options) {
		o.redirectWriter = writer
		o.redirectFilePath = nil
	}
}

// WithFallbackWriter specifies a writer to use if GCP initialization fails.
// If this option is not used, the fallback defaults to os.Stdout.
// This setting is ignored if the LogTarget is not GCP.
func WithFallbackWriter(writer io.Writer) Option {
	return func(o *options) {
		o.fallbackWriter = writer
	}
}

// WithProjectID returns an Option that explicitly sets the Google Cloud Project ID.
func WithProjectID(id string) Option { return func(o *options) { o.projectID = &id } }

// WithParent returns an Option that explicitly sets the GCP resource parent.
func WithParent(parent string) Option { return func(o *options) { o.parent = &parent } }

// WithTraceProjectID returns an Option that sets the project ID for trace resource names.
func WithTraceProjectID(id string) Option { return func(o *options) { o.traceProjectID = &id } }

// WithGCPLogID returns an Option that explicitly sets the Cloud Logging log ID.
func WithGCPLogID(logID string) Option { return func(o *options) { o.gcpLogID = &logID } }

// WithGCPClientScopes returns an Option that sets the OAuth2 scopes for the GCP client.
func WithGCPClientScopes(scopes ...string) Option {
	return func(o *options) {
		if len(scopes) == 0 {
			o.clientScopes = nil
			return
		}
		sCopy := make([]string, len(scopes))
		copy(sCopy, scopes)
		o.clientScopes = sCopy
	}
}

// WithGCPClientOnError returns an Option that sets a custom error handler for
// background errors from the GCP logging client.
func WithGCPClientOnError(f func(error)) Option { return func(o *options) { o.clientOnErrorFunc = f } }

// WithGCPCommonLabel returns an Option that adds a single common label.
func WithGCPCommonLabel(key, value string) Option {
	return func(o *options) {
		if o.programmaticCommonLabels == nil {
			o.programmaticCommonLabels = make(map[string]string)
		}
		o.programmaticCommonLabels[key] = value
	}
}

// WithGCPCommonLabels returns an Option that sets the entire map of common labels.
func WithGCPCommonLabels(labels map[string]string) Option {
	return func(o *options) {
		if len(labels) == 0 {
			o.programmaticCommonLabels = nil
			return
		}
		lCopy := make(map[string]string, len(labels))
		maps.Copy(lCopy, labels)
		o.programmaticCommonLabels = lCopy
	}
}

// WithGCPMonitoredResource returns an Option that explicitly sets the MonitoredResource.
func WithGCPMonitoredResource(res *mrpb.MonitoredResource) Option {
	return func(o *options) { o.gcpMonitoredResource = res }
}

// WithGCPConcurrentWriteLimit returns an Option that sets the number of goroutines for sending logs.
func WithGCPConcurrentWriteLimit(n int) Option {
	return func(o *options) { o.gcpConcurrentWriteLimit = &n }
}

// WithGCPDelayThreshold returns an Option that sets the maximum time the client buffers entries.
func WithGCPDelayThreshold(d time.Duration) Option {
	return func(o *options) { o.gcpDelayThreshold = &d }
}

// WithGCPEntryCountThreshold returns an Option that sets the maximum number of entries buffered.
func WithGCPEntryCountThreshold(n int) Option {
	return func(o *options) { o.gcpEntryCountThreshold = &n }
}

// WithGCPEntryByteThreshold returns an Option that sets the maximum size of entries buffered.
func WithGCPEntryByteThreshold(n int) Option {
	return func(o *options) { o.gcpEntryByteThreshold = &n }
}

// WithGCPEntryByteLimit returns an Option that sets the maximum size of a single log entry.
func WithGCPEntryByteLimit(n int) Option { return func(o *options) { o.gcpEntryByteLimit = &n } }

// WithGCPBufferedByteLimit returns an Option that sets the total memory limit for buffered entries.
func WithGCPBufferedByteLimit(n int) Option { return func(o *options) { o.gcpBufferedByteLimit = &n } }

// WithGCPContextFunc returns an Option that provides a function to generate contexts.
func WithGCPContextFunc(f func() (context.Context, func())) Option {
	return func(o *options) { o.gcpContextFunc = f }
}

// WithGCPDefaultContextTimeout returns an Option that sets a timeout for the default context function.
func WithGCPDefaultContextTimeout(d time.Duration) Option {
	return func(o *options) { o.gcpDefaultContextTimeout = &d }
}

// WithGCPPartialSuccess returns an Option that enables the partial success flag for batch writes.
func WithGCPPartialSuccess(enable bool) Option {
	return func(o *options) { o.gcpPartialSuccess = &enable }
}

// --- Internal Helpers ---

// sourceAwareHandler is a transparent wrapper that advertises HasSource()==true.
type sourceAwareHandler struct{ slog.Handler }

// HasSource reports that sourceAwareHandler wants the slog runtime to capture caller info.
func (h sourceAwareHandler) HasSource() bool { return true }

// wasTargetExplicitlyConfigured determines if the user explicitly specified a logging target.
func wasTargetExplicitlyConfigured(builderState *options, envLogTargetStr, envRedirectTargetStr string) bool {
	if builderState.redirectFilePath != nil || builderState.redirectWriter != nil {
		return true
	}
	if envRedirectTargetStr != "" {
		return true
	}
	if builderState.logTarget != nil && *builderState.logTarget != gcp.LogTargetGCP {
		return true
	}
	if envLogTargetStr != "" && envLogTargetStr != "gcp" {
		return true
	}
	return false
}

// resolveParentAndProjectID determines the final GCP Parent and ProjectID values.
func resolveParentAndProjectID(cfg *gcp.Config, builderState *options, internalLogger *slog.Logger) error {
	if builderState.parent != nil {
		cfg.Parent = *builderState.parent
		if strings.HasPrefix(*builderState.parent, "projects/") {
			cfg.ProjectID = strings.TrimPrefix(*builderState.parent, "projects/")
		} else if builderState.projectID != nil {
			cfg.ProjectID = *builderState.projectID
		}
	} else if builderState.projectID != nil {
		cfg.ProjectID = *builderState.projectID
		if cfg.Parent == "" || strings.HasPrefix(cfg.Parent, "projects/") {
			cfg.Parent = "projects/" + *builderState.projectID
		}
	}

	if cfg.Parent != "" && strings.HasPrefix(cfg.Parent, "projects/") {
		derivedProjectID := strings.TrimPrefix(cfg.Parent, "projects/")
		if cfg.ProjectID == "" {
			cfg.ProjectID = derivedProjectID
		} else if cfg.ProjectID != derivedProjectID {
			internalLogger.Warn("Mismatch between GCP Parent and ProjectID", "parent", cfg.Parent, "projectID", cfg.ProjectID)
		}
	}
	return nil
}

// mergeMaps combines two string maps, with overlay values taking precedence.
func mergeMaps(base, overlay map[string]string) map[string]string {
	if len(overlay) == 0 {
		return base
	}
	if len(base) == 0 {
		return overlay
	}
	merged := make(map[string]string, len(base)+len(overlay))
	maps.Copy(merged, base)
	maps.Copy(merged, overlay)
	return merged
}

// applyProgrammaticOptions merges programmatic configuration options into the provided config.
func applyProgrammaticOptions(cfg *gcp.Config, builderState *options, internalLogger *slog.Logger) error {
	if builderState.logTarget != nil {
		cfg.LogTarget = *builderState.logTarget
		if cfg.LogTarget != gcp.LogTargetStdout && cfg.LogTarget != gcp.LogTargetStderr {
			cfg.RedirectWriter = nil
		}
	}

	if builderState.redirectFilePath != nil {
		if builderState.redirectWriter != nil {
			internalLogger.Warn("Both WithRedirectToFile and WithRedirectWriter used; file path takes precedence", "path", *builderState.redirectFilePath)
		}
		cfg.LogTarget = gcp.LogTargetFile
		cfg.OpenedFilePath = *builderState.redirectFilePath
		cfg.RedirectWriter = nil
		cfg.ClosableUserWriter = nil
	} else if builderState.redirectWriter != nil {
		cfg.RedirectWriter = builderState.redirectWriter
		if c, ok := builderState.redirectWriter.(io.Closer); ok {
			if builderState.redirectWriter != os.Stdout && builderState.redirectWriter != os.Stderr {
				cfg.ClosableUserWriter = c
			}
		}
		cfg.OpenedFilePath = ""
		if builderState.logTarget == nil {
			switch builderState.redirectWriter {
			case os.Stdout:
				cfg.LogTarget = gcp.LogTargetStdout
			case os.Stderr:
				cfg.LogTarget = gcp.LogTargetStderr
			}
		}
	}

	if cfg.OpenedFilePath == "" {
		if cfg.LogTarget == gcp.LogTargetStdout && cfg.RedirectWriter == nil {
			cfg.RedirectWriter = os.Stdout
		}
		if cfg.LogTarget == gcp.LogTargetStderr && cfg.RedirectWriter == nil {
			cfg.RedirectWriter = os.Stderr
		}
	}

	if err := resolveParentAndProjectID(cfg, builderState, internalLogger); err != nil {
		return err
	}

	if builderState.traceProjectID != nil {
		cfg.TraceProjectID = *builderState.traceProjectID
	}
	if builderState.gcpLogID != nil {
		cfg.GCPLogID = *builderState.gcpLogID
	}

	normalizedLogID, err := gcp.NormalizeLogID(cfg.GCPLogID)
	if err != nil {
		return fmt.Errorf("invalid GCP LogID %q: %w", cfg.GCPLogID, err)
	}
	cfg.GCPLogID = normalizedLogID

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

	if len(builderState.middlewares) > 0 {
		cfg.Middlewares = make([]func(slog.Handler) slog.Handler, len(builderState.middlewares))
		for i, mw := range builderState.middlewares {
			cfg.Middlewares[i] = mw
		}
	}

	if builderState.clientScopes != nil {
		cfg.ClientScopes = builderState.clientScopes
	}
	if builderState.clientOnErrorFunc != nil {
		cfg.ClientOnErrorFunc = builderState.clientOnErrorFunc
	}

	cfg.GCPCommonLabels = mergeMaps(cfg.GCPCommonLabels, builderState.programmaticCommonLabels)

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

	if len(builderState.programmaticAttrs) > 0 {
		cfg.InitialAttrs = append([]slog.Attr(nil), builderState.programmaticAttrs...)
	}
	if builderState.programmaticGroup != nil {
		cfg.InitialGroup = *builderState.programmaticGroup
	}

	if cfg.LogTarget == gcp.LogTargetGCP && builderState.redirectWriter != nil {
		return errors.New("conflicting options: LogTarget is GCP but WithRedirectWriter was also used")
	}
	if cfg.LogTarget == gcp.LogTargetFile && cfg.RedirectWriter == nil && cfg.OpenedFilePath == "" {
		return errors.New("log target LogTargetFile requires a file path or a writer")
	}

	return nil
}
