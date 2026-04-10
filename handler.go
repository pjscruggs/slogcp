// Copyright 2025-2026 Patrick J. Scruggs
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
	"strconv"
	"strings"
	"sync"

	"github.com/pjscruggs/slogcp/slogcpasync"
)

const (
	// LabelsGroup is the Cloud Logging attribute group used for structured labels.
	LabelsGroup = "logging.googleapis.com/labels"

	envLogLevel         = "SLOGCP_LEVEL"
	envGenericLogLevel  = "LOG_LEVEL"
	envLogSource        = "SLOGCP_SOURCE_LOCATION"
	envLogTime          = "SLOGCP_TIME"
	envLogStackEnabled  = "SLOGCP_STACK_TRACES"
	envLogStackLevel    = "SLOGCP_STACK_TRACE_LEVEL"
	envSeverityAliases  = "SLOGCP_SEVERITY_ALIASES"
	envTraceProjectID   = "SLOGCP_TRACE_PROJECT_ID"
	envProjectID        = "SLOGCP_PROJECT_ID"
	envGoogleProject    = "GOOGLE_CLOUD_PROJECT"
	envTarget           = "SLOGCP_TARGET"
	envSlogcpGCP        = "SLOGCP_GCP_PROJECT"
	envTraceDiagnostics = "SLOGCP_TRACE_DIAGNOSTICS"
	envAsyncOnFile      = "SLOGCP_ASYNC_ON_FILE"

	traceProjectSourceOption = "WithTraceProjectID"
)

// TraceDiagnosticsMode controls how slogcp surfaces trace correlation issues.
type TraceDiagnosticsMode int

const (
	// TraceDiagnosticsOff disables trace correlation diagnostics.
	TraceDiagnosticsOff TraceDiagnosticsMode = iota
	// TraceDiagnosticsWarnOnce emits a single warning when trace correlation cannot be established.
	TraceDiagnosticsWarnOnce
	// TraceDiagnosticsStrict fails handler construction when trace correlation cannot be established.
	TraceDiagnosticsStrict
)

// CloseTimeoutPolicy controls what [Handler.Close] does when async graceful
// shutdown reaches its flush timeout.
type CloseTimeoutPolicy int

const (
	// CloseTimeoutAutoAbort keeps backward-compatible Close behavior: if
	// graceful async close times out, Close escalates to forceful abort before
	// closing owned resources.
	CloseTimeoutAutoAbort CloseTimeoutPolicy = iota
	// CloseTimeoutReturn keeps Close in graceful-only mode on timeout. Close
	// returns the timeout error and leaves owned resources open so in-flight
	// async workers are not interrupted by a concurrent sink close.
	CloseTimeoutReturn
)

var (
	// ErrInvalidRedirectTarget indicates an unsupported value for SLOGCP_TARGET or redirect options.
	ErrInvalidRedirectTarget = errors.New("slogcp: invalid redirect target")
)

// Option mutates Handler construction behaviour when supplied to [NewHandler].
//
// Options follow the functional options pattern and are applied in the order
// they are provided by the caller.
type Option func(*options)

// Middleware adapts a [slog.Handler] before it is exposed by [Handler].
// Middleware functions run in the order they are supplied, wrapping the core
// handler pipeline from last to first to mirror idiomatic HTTP middleware
// composition.
type Middleware func(slog.Handler) slog.Handler

// Handler routes slog records to Google Cloud Logging with optional
// middlewares, stack traces and trace correlation.
//
// JSON payload emission is best-effort. If a field value cannot be encoded by
// encoding/json, slogcp replaces the failing top-level field with a stable
// "!ERROR:<cause>" placeholder and retries once so one unsupported value does
// not drop the entire entry.
type Handler struct {
	slog.Handler

	cfg              *handlerConfig
	internalLogger   *slog.Logger
	switchableWriter *SwitchableWriter
	ownedFile        *os.File
	levelVar         *slog.LevelVar
	asyncHandler     *slogcpasync.Handler

	mu        sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

type diagLogger interface {
	Printf(format string, args ...any)
}

type traceDiagnosticsLogger struct {
	logger *slog.Logger
}

// Printf writes trace diagnostics through the configured internal logger.
func (l traceDiagnosticsLogger) Printf(format string, args ...any) {
	if l.logger == nil {
		return
	}
	logDiagnostic(l.logger, slog.LevelWarn, fmt.Sprintf(format, args...))
}

type traceDiagnostics struct {
	mode         TraceDiagnosticsMode
	logger       diagLogger
	unknownMu    sync.Once
	invalidMu    sync.Once
	normalizedMu sync.Once
}

// newTraceDiagnostics constructs a traceDiagnostics helper unless tracing is disabled.
func newTraceDiagnostics(mode TraceDiagnosticsMode, logger *slog.Logger) *traceDiagnostics {
	if mode == TraceDiagnosticsOff {
		return nil
	}
	return &traceDiagnostics{
		mode:   mode,
		logger: traceDiagnosticsLogger{logger: ensureInternalLogger(logger)},
	}
}

// warnUnknownProject emits a single warning when we cannot resolve the Cloud project ID.
func (td *traceDiagnostics) warnUnknownProject() {
	if td == nil || td.mode == TraceDiagnosticsOff {
		return
	}
	td.unknownMu.Do(func() {
		if td.logger != nil {
			td.logger.Printf("trace correlation disabled: unable to determine Cloud project ID; set SLOGCP_TRACE_PROJECT_ID/GOOGLE_CLOUD_PROJECT or call slogcp.WithTraceProjectID")
		}
	})
}

// warnInvalidTraceProjectID emits a single warning when an explicit trace project ID is invalid.
func (td *traceDiagnostics) warnInvalidTraceProjectID(value, source string) {
	if td == nil || td.mode != TraceDiagnosticsWarnOnce {
		return
	}
	td.invalidMu.Do(func() {
		if td.logger == nil {
			return
		}
		if source != "" {
			td.logger.Printf("trace correlation disabled: invalid TraceProjectID %q from %s; falling back to runtime detection", value, source)
			return
		}
		td.logger.Printf("trace correlation disabled: invalid TraceProjectID %q; falling back to runtime detection", value)
	})
}

// warnNormalizedTraceProjectID emits a single warning when an explicit trace project ID is normalized.
func (td *traceDiagnostics) warnNormalizedTraceProjectID(value, normalized, source string) {
	if td == nil || td.mode != TraceDiagnosticsWarnOnce {
		return
	}
	td.normalizedMu.Do(func() {
		if td.logger == nil {
			return
		}
		if source != "" {
			td.logger.Printf("trace correlation: normalized TraceProjectID from %q to %q (%s)", value, normalized, source)
			return
		}
		td.logger.Printf("trace correlation: normalized TraceProjectID from %q to %q", value, normalized)
	})
}

type handlerConfig struct {
	Level                    slog.Level
	AddSource                bool
	EmitTimeField            bool
	emitTimeFieldConfigured  bool
	StackTraceEnabled        bool
	StackTraceLevel          slog.Level
	TraceProjectID           string
	traceProjectExplicit     bool
	traceProjectSource       string
	TraceDiagnostics         TraceDiagnosticsMode
	UseShortSeverityNames    bool
	Writer                   io.Writer
	ClosableWriter           io.Closer
	writerExternallyOwned    bool
	FilePath                 string
	ReplaceAttr              func([]string, slog.Attr) slog.Attr
	Middlewares              []Middleware
	InitialAttrs             []slog.Attr
	InitialGroupedAttrs      []groupedAttr
	InitialGroups            []string
	runtimeServiceContext    map[string]string
	runtimeServiceContextAny map[string]any
	traceAllowAutoformat     bool
	traceDiagnosticsState    *traceDiagnostics
	CloseTimeoutPolicy       CloseTimeoutPolicy
}

type options struct {
	level                 *slog.Level
	levelVar              *slog.LevelVar
	addSource             *bool
	emitTimeField         *bool
	stackTraceEnabled     *bool
	stackTraceLevel       *slog.Level
	traceProjectID        *string
	traceDiagnostics      *TraceDiagnosticsMode
	useShortSeverity      *bool
	writer                io.Writer
	writerFilePath        *string
	writerExternallyOwned bool
	replaceAttr           func([]string, slog.Attr) slog.Attr
	middlewares           []Middleware
	attrs                 [][]slog.Attr
	initialGroupedAttrs   []groupedAttr
	groups                []string
	groupsSet             bool
	internalLogger        *slog.Logger
	asyncEnabled          bool
	asyncOnFileTargets    bool
	asyncOpts             []slogcpasync.Option
	additionalHandlers    []slog.Handler
	closeTimeoutPolicy    *CloseTimeoutPolicy
}

// NewHandler builds a Google Cloud aware slog [Handler]. It inspects the
// environment for configuration overrides and then applies any provided
// [Option] values. The handler writes to defaultWriter unless a redirect
// option or environment override is provided. Encoding failures are handled
// with a single sanitize-and-retry pass described in [Handler].
//
// Example:
//
//	h, err := slogcp.NewHandler(os.Stdout,
//		slogcp.WithLevel(slog.LevelInfo),
//	)
//	if err != nil {
//		log.Fatal(err)
//	}
//	logger := slog.New(h)
//	logger.Info("ready")
func NewHandler(defaultWriter io.Writer, opts ...Option) (*Handler, error) {
	builder := collectOptions(opts)
	internalLogger := ensureInternalLogger(builder.internalLogger)

	cfg, err := loadConfigFromEnv(internalLogger)
	if err != nil {
		return nil, err
	}

	applyOptions(&cfg, builder)
	ensureWriterDefaults(&cfg, defaultWriter)
	applyFileTargetTimeDefault(&cfg)

	ownedFile, switchWriter, err := prepareFileWriter(&cfg)
	if err != nil {
		return nil, err
	}

	ensureWriterFallback(&cfg)
	ensureClosableWriter(&cfg)

	levelVar := resolveLevelVar(builder.levelVar, cfg.Level)

	if err := prepareRuntimeConfig(&cfg, internalLogger); err != nil {
		return nil, err
	}

	cfgPtr := &cfg
	handler, asyncHandler := buildPipeline(cfgPtr, levelVar, internalLogger, builder)

	return &Handler{
		Handler:          handler,
		cfg:              cfgPtr,
		internalLogger:   internalLogger,
		switchableWriter: switchWriter,
		ownedFile:        ownedFile,
		levelVar:         levelVar,
		asyncHandler:     asyncHandler,
	}, nil
}

// collectOptions applies provided Option values to a new options struct.
func collectOptions(opts []Option) *options {
	builder := &options{}
	for _, opt := range opts {
		if opt != nil {
			opt(builder)
		}
	}
	return builder
}

// ensureInternalLogger selects logger or defaults to a discard logger.
func ensureInternalLogger(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}
	return slog.New(slog.DiscardHandler)
}

// prepareFileWriter opens file targets and wires them into the handler config.
func prepareFileWriter(cfg *handlerConfig) (*os.File, *SwitchableWriter, error) {
	if cfg.FilePath == "" {
		return nil, nil, nil
	}
	file, err := os.OpenFile(cfg.FilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("slogcp: open log file %q: %w", cfg.FilePath, err)
	}
	switchWriter := NewSwitchableWriter(file)
	cfg.Writer = switchWriter
	cfg.ClosableWriter = nil
	cfg.writerExternallyOwned = false
	return file, switchWriter, nil
}

// ensureClosableWriter records a closer when the writer supports closing.
func ensureClosableWriter(cfg *handlerConfig) {
	if cfg.ClosableWriter != nil || cfg.writerExternallyOwned {
		return
	}
	if c, ok := cfg.Writer.(io.Closer); ok && !isStdStream(cfg.Writer) {
		cfg.ClosableWriter = c
	}
}

// resolveLevelVar builds or updates a slog.LevelVar with the provided level.
func resolveLevelVar(levelVar *slog.LevelVar, level slog.Level) *slog.LevelVar {
	if levelVar == nil {
		levelVar = new(slog.LevelVar)
	}
	levelVar.Set(level)
	return levelVar
}

// prepareRuntimeConfig populates runtime-derived fields and validates tracing.
func prepareRuntimeConfig(cfg *handlerConfig, internalLogger *slog.Logger) error {
	runtimeInfo := DetectRuntimeInfo()
	cfg.runtimeServiceContext = cloneStringMap(runtimeInfo.ServiceContext)
	cfg.runtimeServiceContextAny = stringMapToAny(cfg.runtimeServiceContext)
	cfg.traceDiagnosticsState = newTraceDiagnostics(cfg.TraceDiagnostics, internalLogger)

	if err := normalizeTraceProjectConfig(cfg); err != nil {
		return err
	}
	if cfg.TraceProjectID == "" {
		cfg.TraceProjectID = strings.TrimSpace(runtimeInfo.ProjectID)
	}
	cfg.traceAllowAutoformat = cfg.TraceProjectID == "" && prefersManagedGCPDefaults(runtimeInfo)
	return validateTraceProjectAvailability(cfg)
}

// normalizeTraceProjectConfig validates and normalizes the configured trace project ID.
func normalizeTraceProjectConfig(cfg *handlerConfig) error {
	rawTraceProject := strings.TrimSpace(cfg.TraceProjectID)
	if rawTraceProject == "" {
		cfg.TraceProjectID = ""
		return nil
	}

	normalized, changed, ok := normalizeTraceProjectID(rawTraceProject)
	if !ok {
		return handleInvalidTraceProjectID(cfg, rawTraceProject)
	}

	cfg.TraceProjectID = normalized
	if cfg.traceProjectExplicit && changed {
		cfg.traceDiagnosticsState.warnNormalizedTraceProjectID(rawTraceProject, normalized, cfg.traceProjectSource)
	}
	return nil
}

// handleInvalidTraceProjectID applies diagnostics for invalid trace project IDs.
func handleInvalidTraceProjectID(cfg *handlerConfig, rawTraceProject string) error {
	if cfg.traceProjectExplicit && cfg.TraceDiagnostics == TraceDiagnosticsStrict {
		return invalidTraceProjectIDError(rawTraceProject, cfg.traceProjectSource)
	}
	if cfg.traceProjectExplicit {
		cfg.traceDiagnosticsState.warnInvalidTraceProjectID(rawTraceProject, cfg.traceProjectSource)
	}
	cfg.TraceProjectID = ""
	return nil
}

// invalidTraceProjectIDError formats a strict-mode error for invalid project IDs.
func invalidTraceProjectIDError(value, source string) error {
	if source != "" {
		return fmt.Errorf("slogcp: invalid TraceProjectID %q from %s", value, source)
	}
	return fmt.Errorf("slogcp: invalid TraceProjectID %q", value)
}

// validateTraceProjectAvailability enforces strict mode project ID requirements.
func validateTraceProjectAvailability(cfg *handlerConfig) error {
	if cfg.TraceDiagnostics != TraceDiagnosticsStrict || cfg.TraceProjectID != "" || cfg.traceAllowAutoformat {
		return nil
	}
	return errors.New("slogcp: trace diagnostics strict mode requires a Cloud project ID; set SLOGCP_TRACE_PROJECT_ID or provide slogcp.WithTraceProjectID")
}

// buildPipeline assembles the handler stack with middlewares and async wrapper.
func buildPipeline(cfg *handlerConfig, levelVar *slog.LevelVar, internalLogger *slog.Logger, builder *options) (slog.Handler, *slogcpasync.Handler) {
	core := slog.Handler(newJSONHandler(cfg, levelVar, internalLogger))
	handler := composeFanout(core, builder.additionalHandlers, levelVar)
	handler = applyMiddlewares(handler, cfg.Middlewares)

	isFileTarget := hasFileTarget(cfg)
	asyncEnabled, asyncOnFileTargets := resolveAsyncConfig(isFileTarget, builder)

	var asyncHandler *slogcpasync.Handler
	if asyncEnabled && (!asyncOnFileTargets || isFileTarget) {
		wrapped := slogcpasync.Wrap(handler, builder.asyncOpts...)
		if ah, ok := wrapped.(*slogcpasync.Handler); ok {
			asyncHandler = ah
		}
		handler = wrapped
	}
	if cfg.AddSource {
		handler = sourceAwareHandler{Handler: handler}
	}
	return handler, asyncHandler
}

// composeFanout composes the primary handler with optional additional sinks.
// Fan-out dispatch and error aggregation are delegated to slog.NewMultiHandler,
// which invokes all enabled sinks and returns errors.Join of sink failures.
func composeFanout(primary slog.Handler, additional []slog.Handler, leveler slog.Leveler) slog.Handler {
	if len(additional) == 0 {
		return primary
	}
	handlers := make([]slog.Handler, 0, len(additional)+1)
	handlers = append(handlers, primary)
	for _, h := range additional {
		if h != nil {
			handlers = append(handlers, sharedLevelHandler{Handler: h, leveler: leveler})
		}
	}
	if len(handlers) == 1 {
		return primary
	}
	return slog.NewMultiHandler(handlers...)
}

// applyMiddlewares wraps handler with supplied middlewares from last to first.
func applyMiddlewares(handler slog.Handler, middlewares []Middleware) slog.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		handler = middlewares[i](handler)
	}
	return handler
}

// sharedLevelHandler applies slogcp's shared level threshold to wrapped sinks.
// It preserves the full slog.Handler contract (Enabled/Handle/WithAttrs/WithGroup)
// while adding level gating for additional fan-out handlers.
type sharedLevelHandler struct {
	slog.Handler
	leveler slog.Leveler
}

// Enabled reports whether the level satisfies slogcp's shared threshold and the wrapped handler.
func (h sharedLevelHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if h.leveler != nil && level < h.leveler.Level() {
		return false
	}
	return h.Handler.Enabled(ctx, level)
}

// Handle drops records below the shared level before forwarding.
// Errors from wrapped handlers are returned with context using %w so callers
// can still match concrete sink errors via errors.Is/errors.As.
func (h sharedLevelHandler) Handle(ctx context.Context, r slog.Record) error {
	if !h.Enabled(ctx, r.Level) {
		return nil
	}
	if err := h.Handler.Handle(ctx, r); err != nil {
		return fmt.Errorf("shared level handler: %w", err)
	}
	return nil
}

// WithAttrs propagates attribute state while preserving shared level gating.
// Returning sharedLevelHandler ensures derived fan-out handlers remain level-aware.
func (h sharedLevelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	child := h.Handler.WithAttrs(attrs)
	if child == nil {
		return nil
	}
	return sharedLevelHandler{Handler: child, leveler: h.leveler}
}

// WithGroup propagates groups while preserving shared level gating.
// Returning sharedLevelHandler ensures derived fan-out handlers remain level-aware.
func (h sharedLevelHandler) WithGroup(name string) slog.Handler {
	child := h.Handler.WithGroup(name)
	if child == nil {
		return nil
	}
	return sharedLevelHandler{Handler: child, leveler: h.leveler}
}

// resolveAsyncConfig determines async wrapper defaults for file targets when no explicit option is set.
func resolveAsyncConfig(isFileTarget bool, builder *options) (bool, bool) {
	if builder.asyncEnabled {
		return true, builder.asyncOnFileTargets
	}
	if !isFileTarget {
		return false, false
	}
	enabled, ok := asyncOnFileTargetsFromEnv()
	if ok && !enabled {
		return false, false
	}
	return true, true
}

// asyncOnFileTargetsFromEnv reads SLOGCP_ASYNC_ON_FILE to enable or disable file buffering.
func asyncOnFileTargetsFromEnv() (bool, bool) {
	raw := strings.TrimSpace(os.Getenv(envAsyncOnFile))
	if raw == "" {
		return false, false
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return false, false
	}
	return enabled, true
}

// ensureWriterDefaults assigns cfg.Writer when unset by falling back to either
// the provided defaultWriter or os.Stdout. It also marks the writer as
// externally owned so Close does not manage it.
func ensureWriterDefaults(cfg *handlerConfig, defaultWriter io.Writer) {
	if cfg.Writer == nil && cfg.FilePath == "" {
		if defaultWriter != nil {
			cfg.Writer = defaultWriter
		} else {
			cfg.Writer = os.Stdout
		}
		cfg.writerExternallyOwned = true
	}
}

// applyFileTargetTimeDefault enables timestamp emission when logging to files unless the user
// explicitly set WithTime/SLOGCP_TIME.
func applyFileTargetTimeDefault(cfg *handlerConfig) {
	if cfg.emitTimeFieldConfigured {
		return
	}
	if hasFileTarget(cfg) {
		cfg.EmitTimeField = true
	}
}

// hasFileTarget reports whether the handler is configured to write to a file rather than stdout/stderr.
func hasFileTarget(cfg *handlerConfig) bool {
	if cfg == nil {
		return false
	}
	if cfg.FilePath != "" {
		return true
	}
	return writerIsFileTarget(cfg.Writer, 0)
}

// writerIsFileTarget inspects writer plumbing (including SwitchableWriter) to detect file handles.
func writerIsFileTarget(w io.Writer, depth int) bool {
	if w == nil || depth > 1 {
		return false
	}
	if f, ok := w.(*os.File); ok && f != nil && !isStdStream(f) {
		return true
	}
	if sw, ok := w.(*SwitchableWriter); ok {
		next := sw.GetCurrentWriter()
		if next == w {
			return false
		}
		return writerIsFileTarget(next, depth+1)
	}
	return false
}

// ensureWriterFallback guarantees cfg.Writer is non-nil even when no writer
// options were configured.
func ensureWriterFallback(cfg *handlerConfig) {
	if cfg.Writer != nil {
		return
	}
	cfg.Writer = os.Stdout
	cfg.writerExternallyOwned = true
}

// Close releases any resources owned by the handler such as log files or
// writer implementations created by options. It first runs async shutdown via
// the configured flush timeout.
//
// On timeout, behavior depends on [CloseTimeoutPolicy]:
//   - [CloseTimeoutAutoAbort] (default): Close escalates to abort, then closes
//     owned resources.
//   - [CloseTimeoutReturn]: Close returns timeout errors without aborting and
//     leaves owned resources open.
func (h *Handler) Close() error {
	if h == nil {
		return nil
	}

	asyncErr := h.closeAsyncHandler()
	if errors.Is(asyncErr, slogcpasync.ErrFlushTimeout) {
		if h.closeTimeoutPolicy() == CloseTimeoutAutoAbort {
			asyncErr = errors.Join(asyncErr, h.abortAsyncHandler(context.Background()))
			return errors.Join(asyncErr, h.closeResourcesOnce())
		}
		// Do not close owned resources here: async workers may still be in
		// downstream writes, and closing sinks underneath them can corrupt output
		// or trigger avoidable write/close races in custom writers.
		return asyncErr
	}

	return errors.Join(asyncErr, h.closeResourcesOnce())
}

// closeTimeoutPolicy resolves Close timeout behavior, defaulting invalid or
// unspecified values to CloseTimeoutAutoAbort for backward compatibility.
func (h *Handler) closeTimeoutPolicy() CloseTimeoutPolicy {
	if h == nil || h.cfg == nil {
		return CloseTimeoutAutoAbort
	}
	return normalizeCloseTimeoutPolicy(h.cfg.CloseTimeoutPolicy)
}

// lifecycleContext preserves the handler's defensive nil-context behavior for
// shutdown paths by treating nil the same as context.Background().
func lifecycleContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// Shutdown performs graceful async drain bounded by ctx. On timeout it returns
// without tearing down owned resources so callers can decide whether to abort.
// A nil ctx is treated as context.Background().
func (h *Handler) Shutdown(ctx context.Context) error {
	if h == nil {
		return nil
	}

	if err := h.shutdownAsyncHandler(ctx); err != nil {
		return err
	}
	return h.closeResourcesOnce()
}

// Abort performs forceful async shutdown (bounded by ctx) and then closes
// owned resources. When ctx expires first, Abort returns promptly while the
// underlying async abort may still be finishing in the background. A nil ctx is
// treated as context.Background().
func (h *Handler) Abort(ctx context.Context) error {
	if h == nil {
		return nil
	}

	asyncErr := h.abortAsyncHandler(ctx)
	return errors.Join(asyncErr, h.closeResourcesOnce())
}

// closeAsyncHandler drains any async wrapper so queued records are written before resources are closed.
func (h *Handler) closeAsyncHandler() error {
	if h == nil || h.asyncHandler == nil {
		return nil
	}
	if err := h.asyncHandler.Close(); err != nil {
		return fmt.Errorf("close async handler: %w", err)
	}
	return nil
}

// shutdownAsyncHandler asks the async wrapper to drain until ctx expires.
func (h *Handler) shutdownAsyncHandler(ctx context.Context) error {
	if h == nil || h.asyncHandler == nil {
		return nil
	}
	if err := h.asyncHandler.Shutdown(lifecycleContext(ctx)); err != nil {
		return fmt.Errorf("shutdown async handler: %w", err)
	}
	return nil
}

// abortAsyncHandler requests forceful async shutdown and waits until either the
// abort completes or ctx expires.
//
// This intentionally delegates bounded waiting to slogcpasync.AbortContext so
// repeated timeout callers share one abort coordinator instead of spawning a
// helper goroutine per call.
func (h *Handler) abortAsyncHandler(ctx context.Context) error {
	if h == nil || h.asyncHandler == nil {
		return nil
	}

	if err := h.asyncHandler.AbortContext(lifecycleContext(ctx)); err != nil {
		return fmt.Errorf("abort async handler: %w", err)
	}
	return nil
}

// closeResources tears down owned writers and closers.
func (h *Handler) closeResources() error {
	h.mu.Lock()
	closeOwnedFile := h.ownedFile != nil
	firstErr := error(nil)

	closeOwnedFile, firstErr = h.closeSwitchableWriter(closeOwnedFile, firstErr)
	firstErr = h.closeOwnedFile(closeOwnedFile, firstErr)
	h.ownedFile = nil
	h.mu.Unlock()

	if err := h.closeConfiguredWriter(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// closeResourcesOnce tears down owned resources exactly once and returns the
// cached result on subsequent calls.
func (h *Handler) closeResourcesOnce() error {
	if h == nil {
		return nil
	}
	h.closeOnce.Do(func() {
		h.closeErr = h.closeResources()
	})
	return h.closeErr
}

// closeSwitchableWriter closes the switchable writer and determines if the file should be closed.
func (h *Handler) closeSwitchableWriter(closeOwnedFile bool, firstErr error) (bool, error) {
	if h.switchableWriter == nil {
		return closeOwnedFile, firstErr
	}
	if err := h.switchableWriter.Close(); err != nil && firstErr == nil {
		firstErr = err
		h.internalLogger.Error("failed to close switchable writer", slog.Any("error", err))
	} else if err == nil {
		closeOwnedFile = false
	}
	h.switchableWriter = nil
	return closeOwnedFile, firstErr
}

// closeOwnedFile closes the owned file when it still needs closing.
func (h *Handler) closeOwnedFile(closeOwnedFile bool, firstErr error) error {
	if !closeOwnedFile || h.ownedFile == nil {
		return firstErr
	}
	if err := h.ownedFile.Close(); err != nil && firstErr == nil {
		firstErr = err
		h.internalLogger.Error("failed to close log file", slog.Any("error", err))
	}
	return firstErr
}

// closeConfiguredWriter closes any ClosableWriter configured on the handler.
func (h *Handler) closeConfiguredWriter() error {
	if h.cfg == nil || h.cfg.ClosableWriter == nil {
		return nil
	}
	if err := h.cfg.ClosableWriter.Close(); err != nil {
		h.internalLogger.Error("failed to close writer", slog.Any("error", err))
		return fmt.Errorf("close writer: %w", err)
	}
	return nil
}

// ReopenLogFile rotates the handler's file writer when logging to a file.
// If the handler is not writing to a file the method is a no-op.
func (h *Handler) ReopenLogFile() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.cfg == nil || h.cfg.FilePath == "" || h.switchableWriter == nil {
		return nil
	}

	file, err := os.OpenFile(h.cfg.FilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("slogcp: reopen log file %q: %w", h.cfg.FilePath, err)
	}

	oldFile := h.ownedFile
	h.ownedFile = file
	h.switchableWriter.SetWriter(file)
	if oldFile != nil {
		if err := oldFile.Close(); err != nil {
			h.internalLogger.Warn("error closing log file before reopen", slog.Any("error", err))
		}
	}
	return nil
}

// SetLevel updates the minimum slog level accepted by the handler at runtime.
// Calls are safe for concurrent use.
func (h *Handler) SetLevel(level slog.Level) {
	if h == nil || h.levelVar == nil {
		return
	}
	h.levelVar.Set(level)
}

// Level reports the handler's current minimum slog level.
func (h *Handler) Level() slog.Level {
	if h == nil || h.levelVar == nil {
		return slog.LevelInfo
	}
	return h.levelVar.Level()
}

// LevelVar returns the underlying slog.LevelVar used to gate records. It can
// be integrated with external configuration systems for dynamic level control.
func (h *Handler) LevelVar() *slog.LevelVar {
	if h == nil {
		return nil
	}
	return h.levelVar
}

// Default logs a structured message at [LevelDefault] severity without requiring
// a context value.
func Default(logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(context.Background(), LevelDefault.Level(), msg, args...)
}

// Debug logs a structured message at LevelDebug severity without requiring a
// context. It mirrors slog.Logger.Debug while guaranteeing Cloud Logging
// severity alignment.
func Debug(logger *slog.Logger, msg string, args ...any) {
	DebugContext(context.Background(), logger, msg, args...)
}

// DefaultContext logs a structured message at [LevelDefault] severity while
// attaching contextual attributes from ctx. It is suitable for routing
// application level events through Cloud Logging.
func DefaultContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(ctx, LevelDefault.Level(), msg, args...)
}

// DebugContext logs a message at LevelDebug severity while attaching
// contextual attributes from ctx.
func DebugContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(ctx, LevelDebug.Level(), msg, args...)
}

// Info logs a structured message at LevelInfo severity without requiring a
// context, mirroring slog.Logger.Info.
func Info(logger *slog.Logger, msg string, args ...any) {
	InfoContext(context.Background(), logger, msg, args...)
}

// InfoContext logs a message at LevelInfo severity while attaching contextual
// attributes from ctx.
func InfoContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(ctx, LevelInfo.Level(), msg, args...)
}

// Warn logs a structured message at LevelWarn severity without requiring a
// context, mirroring slog.Logger.Warn.
func Warn(logger *slog.Logger, msg string, args ...any) {
	WarnContext(context.Background(), logger, msg, args...)
}

// WarnContext logs a message at LevelWarn severity while attaching contextual
// attributes from ctx.
func WarnContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(ctx, LevelWarn.Level(), msg, args...)
}

// Error logs a structured message at LevelError severity without requiring a
// context, mirroring slog.Logger.Error.
func Error(logger *slog.Logger, msg string, args ...any) {
	ErrorContext(context.Background(), logger, msg, args...)
}

// ErrorContext logs a message at LevelError severity while attaching
// contextual attributes from ctx.
func ErrorContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(ctx, LevelError.Level(), msg, args...)
}

// NoticeContext logs a message at notice severity for operational events that
// should page responders but do not indicate an outage.
func NoticeContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(ctx, LevelNotice.Level(), msg, args...)
}

// CriticalContext logs a message at critical severity indicating immediate
// attention is required.
func CriticalContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(ctx, LevelCritical.Level(), msg, args...)
}

// AlertContext logs at alert severity to integrate with on-call workflows for
// non-recoverable issues.
func AlertContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(ctx, LevelAlert.Level(), msg, args...)
}

// EmergencyContext logs at emergency severity highlighting application-wide
// failures that demand instant response.
func EmergencyContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(ctx, LevelEmergency.Level(), msg, args...)
}

// WithInternalLogger injects an internal logger used for diagnostics during
// handler setup and lifecycle operations.
func WithInternalLogger(logger *slog.Logger) Option {
	return func(o *options) {
		o.internalLogger = logger
	}
}

// WithLevel sets the minimum slog level accepted by the handler.
func WithLevel(level slog.Level) Option {
	return func(o *options) {
		o.level = &level
	}
}

// WithLevelVar shares the provided slog.LevelVar with the handler, allowing
// external code to adjust log levels at runtime while keeping slogcp's internal
// state in sync. When supplied, slogcp initializes the LevelVar to the
// resolved minimum level (defaults, environment, then WithLevel) during
// handler construction.
func WithLevelVar(levelVar *slog.LevelVar) Option {
	return func(o *options) {
		if levelVar != nil {
			o.levelVar = levelVar
		}
	}
}

// WithSourceLocationEnabled toggles source location enrichment on emitted
// records when true.
func WithSourceLocationEnabled(enabled bool) Option {
	return func(o *options) {
		o.addSource = &enabled
	}
}

// WithTime toggles emission of the top-level "time" field. By default slogcp
// omits timestamps on managed GCP runtimes such as Cloud Run or App Engine
// when writing to stdout/stderr, since Cloud Logging stamps entries
// automatically. File targets keep timestamps by default so rotated or shipped
// logs retain them. Enabling this option stops slogcp from suppressing
// `log/slog`'s default timestamp behavior.
func WithTime(enabled bool) Option {
	return func(o *options) {
		o.emitTimeField = &enabled
	}
}

// WithStackTraceEnabled toggles automatic stack trace capture for error logs
// when enabled.
func WithStackTraceEnabled(enabled bool) Option {
	return func(o *options) {
		o.stackTraceEnabled = &enabled
	}
}

// WithStackTraceLevel captures stack traces for records at or above level.
// The handler defaults to [slog.LevelError] when this option is omitted.
func WithStackTraceLevel(level slog.Level) Option {
	return func(o *options) {
		o.stackTraceLevel = &level
	}
}

// WithTraceProjectID overrides the Cloud Trace project identifier used to
// format [logging.googleapis.com/trace] URLs.
func WithTraceProjectID(id string) Option {
	trimmed := strings.TrimSpace(id)
	return func(o *options) {
		o.traceProjectID = &trimmed
	}
}

// WithTraceDiagnostics adjusts how slogcp reports trace correlation issues.
func WithTraceDiagnostics(mode TraceDiagnosticsMode) Option {
	return func(o *options) {
		o.traceDiagnostics = &mode
	}
}

// WithSeverityAliases configures whether the handler emits Cloud Logging severity
// aliases (for example, "I" for INFO) instead of the full severity names. Using
// the aliases trims roughly a nanosecond from JSON marshaling, and Cloud
// Logging still displays the canonical severity names after ingestion. On a
// detected GCP runtime the aliases remain enabled by default; otherwise they
// must be explicitly enabled through options or environment variables.
func WithSeverityAliases(enabled bool) Option {
	return func(o *options) {
		o.useShortSeverity = &enabled
	}
}

// WithRedirectToStdout forces the handler to emit logs to stdout.
func WithRedirectToStdout() Option {
	return func(o *options) {
		o.writer = os.Stdout
		o.writerFilePath = nil
		o.writerExternallyOwned = true
	}
}

// WithRedirectToStderr forces the handler to emit logs to stderr.
func WithRedirectToStderr() Option {
	return func(o *options) {
		o.writer = os.Stderr
		o.writerFilePath = nil
		o.writerExternallyOwned = true
	}
}

// WithRedirectToFile directs handler output to the file at path, creating it
// if necessary. The path is trimmed of surrounding whitespace and then passed
// verbatim to os.OpenFile in append mode; parent directories must already
// exist. When configuring the same behaviour via SLOGCP_TARGET use "file:<path>"
// with an OS-specific path string (for example, "file:/var/log/app.log" or
// "file:C:\\logs\\app.log").
func WithRedirectToFile(path string) Option {
	trimmed := strings.TrimSpace(path)
	return func(o *options) {
		o.writer = nil
		o.writerFilePath = &trimmed
		o.writerExternallyOwned = false
	}
}

// WithRedirectWriter uses writer for log output without taking ownership of
// its lifecycle. Any file paths configured on the writer itself are interpreted
// by that writer; slogcp does not parse "file:" targets when this option is
// used.
func WithRedirectWriter(writer io.Writer) Option {
	return func(o *options) {
		o.writer = writer
		o.writerFilePath = nil
		o.writerExternallyOwned = true
	}
}

// WithReplaceAttr installs a slog attribute replacer mirroring
// [slog.HandlerOptions.ReplaceAttr].
func WithReplaceAttr(fn func([]string, slog.Attr) slog.Attr) Option {
	return func(o *options) {
		o.replaceAttr = fn
	}
}

// WithMiddleware appends a middleware that can modify or short-circuit record
// handling.
func WithMiddleware(mw Middleware) Option {
	return func(o *options) {
		if mw != nil {
			o.middlewares = append(o.middlewares, mw)
		}
	}
}

// WithAdditionalHandlers fans out accepted records to additional handlers using
// [slog.NewMultiHandler]. Nil handlers are ignored.
//
// Additional handlers are not owned by slogcp. If they require shutdown
// (for example, async wrappers), close them explicitly.
// Additional sinks are wrapped with sharedLevelHandler so they use slogcp's
// shared level threshold. When a sink fails, the wrapped error remains
// unwrappable (errors.Is/errors.As) and MultiHandler aggregates sink failures.
func WithAdditionalHandlers(handlers ...slog.Handler) Option {
	return func(o *options) {
		for _, h := range handlers {
			if h != nil {
				o.additionalHandlers = append(o.additionalHandlers, h)
			}
		}
	}
}

// WithAsync wraps the constructed handler in slogcpasync using tuned per-mode defaults.
// Supply slogcpasync options to override queue, workers, drop mode, or batch size.
func WithAsync(opts ...slogcpasync.Option) Option {
	return func(o *options) {
		o.asyncEnabled = true
		o.asyncOnFileTargets = false
		o.asyncOpts = append(o.asyncOpts, opts...)
	}
}

// WithAsyncOnFile applies slogcpasync only when logging to a file target.
// It keeps stdout/stderr handlers synchronous while letting callers tune (or disable) file buffering.
func WithAsyncOnFile(opts ...slogcpasync.Option) Option {
	return func(o *options) {
		o.asyncEnabled = true
		o.asyncOnFileTargets = true
		o.asyncOpts = append(o.asyncOpts, opts...)
	}
}

// WithCloseTimeoutPolicy configures how [Handler.Close] behaves when async
// graceful shutdown reaches its flush timeout.
//
// The default is [CloseTimeoutAutoAbort]. Use [CloseTimeoutReturn] to make
// Close return timeout errors without aborting or closing owned resources.
func WithCloseTimeoutPolicy(policy CloseTimeoutPolicy) Option {
	normalized := normalizeCloseTimeoutPolicy(policy)
	return func(o *options) {
		o.closeTimeoutPolicy = &normalized
	}
}

// WithAttrs preloads static attributes to be attached to every record emitted
// by the handler.
func WithAttrs(attrs []slog.Attr) Option {
	return func(o *options) {
		if len(attrs) == 0 {
			return
		}
		dup := make([]slog.Attr, len(attrs))
		copy(dup, attrs)
		currentGroups := append([]string(nil), o.groups...)
		for _, attr := range dup {
			o.initialGroupedAttrs = append(o.initialGroupedAttrs, groupedAttr{
				groups: currentGroups,
				attr:   attr,
			})
		}
		o.attrs = append(o.attrs, dup)
	}
}

// WithGroup nests subsequent attributes under the supplied group name.
func WithGroup(name string) Option {
	trimmed := strings.TrimSpace(name)
	return func(o *options) {
		o.groupsSet = true
		if trimmed == "" {
			o.groups = nil
			return
		}
		o.groups = append(o.groups, trimmed)
	}
}

// prefersManagedGCPDefaults reports whether the detected runtime should use
// managed Google Cloud defaults for logging configuration.
func prefersManagedGCPDefaults(info RuntimeInfo) bool {
	switch info.Environment {
	case RuntimeEnvCloudRunService,
		RuntimeEnvCloudRunJob,
		RuntimeEnvCloudFunctions,
		RuntimeEnvAppEngineStandard,
		RuntimeEnvAppEngineFlexible:
		return true
	default:
		return false
	}
}

// defaultUseShortSeverityNames reports whether Cloud Logging severity aliases
// should be enabled implicitly for the current runtime.
func defaultUseShortSeverityNames() bool {
	return prefersManagedGCPDefaults(DetectRuntimeInfo())
}

// defaultEmitTimeField reports whether slogcp should emit the top-level time
// field without any explicit configuration from the caller.
func defaultEmitTimeField() bool {
	return !prefersManagedGCPDefaults(DetectRuntimeInfo())
}

// resolveTraceProjectFromEnv returns the highest-priority trace project ID and its source.
func resolveTraceProjectFromEnv() (string, string) {
	candidates := []string{
		envTraceProjectID,
		envProjectID,
		envSlogcpGCP,
		envGoogleProject,
	}
	for _, name := range candidates {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value, name
		}
	}
	return "", ""
}

// loadConfigFromEnv reads handler configuration overrides from environment
// variables, returning the assembled configuration and any validation errors.
func loadConfigFromEnv(logger *slog.Logger) (handlerConfig, error) {
	cfg := handlerConfig{
		Level:                 slog.LevelInfo,
		StackTraceLevel:       slog.LevelError,
		EmitTimeField:         defaultEmitTimeField(),
		TraceDiagnostics:      TraceDiagnosticsWarnOnce,
		UseShortSeverityNames: defaultUseShortSeverityNames(),
		CloseTimeoutPolicy:    CloseTimeoutAutoAbort,
	}

	cfg.Level = resolveLevelFromEnv(cfg.Level, logger)
	cfg.AddSource = parseBoolEnv(os.Getenv(envLogSource), cfg.AddSource, logger)
	cfg.EmitTimeField, cfg.emitTimeFieldConfigured = parseBoolEnvWithPresence(os.Getenv(envLogTime), cfg.EmitTimeField, logger)
	cfg.StackTraceEnabled = parseBoolEnv(os.Getenv(envLogStackEnabled), cfg.StackTraceEnabled, logger)
	cfg.StackTraceLevel = parseLevelEnv(os.Getenv(envLogStackLevel), cfg.StackTraceLevel, logger)
	cfg.UseShortSeverityNames = parseBoolEnv(os.Getenv(envSeverityAliases), cfg.UseShortSeverityNames, logger)
	cfg.TraceDiagnostics = parseTraceDiagnosticsEnv(os.Getenv(envTraceDiagnostics), cfg.TraceDiagnostics, logger)

	traceProject, source := resolveTraceProjectFromEnv()
	cfg.TraceProjectID = traceProject
	cfg.traceProjectSource = source
	cfg.traceProjectExplicit = traceProject != ""

	if err := applyTargetFromEnv(&cfg, logger); err != nil {
		return handlerConfig{}, err
	}

	return cfg, nil
}

// applyOptions merges user-supplied options into the derived handler
// configuration.
func applyOptions(cfg *handlerConfig, o *options) {
	applyLevelAndTraceOptions(cfg, o)
	applyWriterOptions(cfg, o)
	applyAttrOptions(cfg, o)
	applyGroupOptions(cfg, o)
}

// applyLevelAndTraceOptions overlays level and tracing options onto cfg.
func applyLevelAndTraceOptions(cfg *handlerConfig, o *options) {
	if o.level != nil {
		cfg.Level = *o.level
	}
	if o.addSource != nil {
		cfg.AddSource = *o.addSource
	}
	if o.emitTimeField != nil {
		cfg.EmitTimeField = *o.emitTimeField
		cfg.emitTimeFieldConfigured = true
	}
	if o.stackTraceEnabled != nil {
		cfg.StackTraceEnabled = *o.stackTraceEnabled
	}
	if o.stackTraceLevel != nil {
		cfg.StackTraceLevel = *o.stackTraceLevel
	}
	if o.traceProjectID != nil {
		cfg.TraceProjectID = *o.traceProjectID
		cfg.traceProjectExplicit = true
		cfg.traceProjectSource = traceProjectSourceOption
	}
	if o.traceDiagnostics != nil {
		cfg.TraceDiagnostics = *o.traceDiagnostics
	}
	if o.useShortSeverity != nil {
		cfg.UseShortSeverityNames = *o.useShortSeverity
	}
	if o.closeTimeoutPolicy != nil {
		cfg.CloseTimeoutPolicy = normalizeCloseTimeoutPolicy(*o.closeTimeoutPolicy)
	}
}

// normalizeCloseTimeoutPolicy maps unknown values to the default
// backward-compatible behavior.
func normalizeCloseTimeoutPolicy(policy CloseTimeoutPolicy) CloseTimeoutPolicy {
	switch policy {
	case CloseTimeoutAutoAbort, CloseTimeoutReturn:
		return policy
	default:
		return CloseTimeoutAutoAbort
	}
}

// applyWriterOptions configures writer targets from options.
func applyWriterOptions(cfg *handlerConfig, o *options) {
	if o.writerFilePath != nil {
		cfg.FilePath = strings.TrimSpace(*o.writerFilePath)
		cfg.Writer = nil
		cfg.ClosableWriter = nil
		cfg.writerExternallyOwned = false
	}
	if o.writer != nil {
		cfg.Writer = o.writer
		cfg.FilePath = ""
		cfg.ClosableWriter = nil
		cfg.writerExternallyOwned = o.writerExternallyOwned
	}
}

// applyAttrOptions merges middleware and initial attribute options.
func applyAttrOptions(cfg *handlerConfig, o *options) {
	if o.replaceAttr != nil {
		cfg.ReplaceAttr = o.replaceAttr
	}
	if len(o.middlewares) > 0 {
		cfg.Middlewares = append([]Middleware(nil), o.middlewares...)
	}
	if len(o.attrs) > 0 {
		for _, group := range o.attrs {
			cfg.InitialAttrs = append(cfg.InitialAttrs, group...)
		}
	}
	if len(o.initialGroupedAttrs) > 0 {
		for _, ga := range o.initialGroupedAttrs {
			cfg.InitialGroupedAttrs = append(cfg.InitialGroupedAttrs, groupedAttr{
				groups: append([]string(nil), ga.groups...),
				attr:   ga.attr,
			})
		}
	}
}

// applyGroupOptions carries initial groups from options.
func applyGroupOptions(cfg *handlerConfig, o *options) {
	if o.groupsSet {
		cfg.InitialGroups = append([]string(nil), o.groups...)
	}
}

// applyTargetFromEnv adjusts the output destination based on SLOGCP_TARGET.
func applyTargetFromEnv(cfg *handlerConfig, logger *slog.Logger) error {
	target := strings.TrimSpace(os.Getenv(envTarget))
	if target == "" {
		return nil
	}

	lower := strings.ToLower(target)
	switch {
	case lower == "stdout":
		cfg.Writer = os.Stdout
		cfg.FilePath = ""
		cfg.ClosableWriter = nil
		cfg.writerExternallyOwned = true
	case lower == "stderr":
		cfg.Writer = os.Stderr
		cfg.FilePath = ""
		cfg.ClosableWriter = nil
		cfg.writerExternallyOwned = true
	case strings.HasPrefix(lower, "file:"):
		path := strings.TrimSpace(target[len("file:"):])
		if path == "" {
			logDiagnostic(logger, slog.LevelWarn, "empty file target", slog.String("variable", envTarget))
			return ErrInvalidRedirectTarget
		}
		cfg.FilePath = path
		cfg.Writer = nil
		cfg.ClosableWriter = nil
		cfg.writerExternallyOwned = false
	default:
		logDiagnostic(logger, slog.LevelWarn, "unknown SLOGCP_TARGET", slog.String("value", target))
		return ErrInvalidRedirectTarget
	}

	return nil
}

// parseBoolEnv interprets truthy environment variable values with validation
// diagnostics.
func parseBoolEnv(value string, current bool, logger *slog.Logger) bool {
	val, _ := parseBoolEnvWithPresence(value, current, logger)
	return val
}

// parseBoolEnvWithPresence parses boolean environment variables and reports whether a value was present.
func parseBoolEnvWithPresence(value string, current bool, logger *slog.Logger) (bool, bool) {
	if strings.TrimSpace(value) == "" {
		return current, false
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		logDiagnostic(logger, slog.LevelWarn, "invalid boolean environment variable", slog.String("value", value), slog.Any("error", err))
		return current, false
	}
	return b, true
}

// resolveLevelFromEnv selects a log level from slogcp environment variables,
// preferring SLOGCP_LEVEL and falling back to LOG_LEVEL when the slogcp-specific
// variable is unset or blank.
func resolveLevelFromEnv(current slog.Level, logger *slog.Logger) slog.Level {
	level := strings.TrimSpace(os.Getenv(envLogLevel))
	if level != "" {
		return parseLevelEnv(level, current, logger)
	}
	return parseLevelEnv(os.Getenv(envGenericLogLevel), current, logger)
}

var envLevelAliases = map[string]slog.Level{
	"debug":     slog.LevelDebug,
	"info":      slog.LevelInfo,
	"warn":      slog.LevelWarn,
	"warning":   slog.LevelWarn,
	"error":     slog.LevelError,
	"default":   slog.Level(LevelDefault),
	"notice":    slog.Level(LevelNotice),
	"critical":  slog.Level(LevelCritical),
	"alert":     slog.Level(LevelAlert),
	"emergency": slog.Level(LevelEmergency),
}

// parseLevelEnv parses slog levels from environment variables, retaining the
// current level on failure.
func parseLevelEnv(value string, current slog.Level, logger *slog.Logger) slog.Level {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		return current
	}

	if lv, ok := envLevelAliases[trimmed]; ok {
		return lv
	}
	if lv, err := strconv.Atoi(trimmed); err == nil {
		return slog.Level(lv)
	}

	logDiagnostic(logger, slog.LevelWarn, "invalid log level environment variable", slog.String("value", value))
	return current
}

// parseTraceDiagnosticsEnv validates diagnostics mode overrides from env vars.
func parseTraceDiagnosticsEnv(value string, current TraceDiagnosticsMode, logger *slog.Logger) TraceDiagnosticsMode {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		return current
	}

	switch trimmed {
	case "off", "disable", "disabled", "none":
		return TraceDiagnosticsOff
	case "warn", "warn_once", "warn-once", "warnonce":
		return TraceDiagnosticsWarnOnce
	case "strict":
		return TraceDiagnosticsStrict
	default:
		logDiagnostic(logger, slog.LevelWarn, "invalid trace diagnostics mode", slog.String("value", value))
		return current
	}
}

// isStdStream reports whether w is stdout or stderr.
func isStdStream(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || f == nil {
		return false
	}
	return f.Fd() == os.Stdout.Fd() || f.Fd() == os.Stderr.Fd()
}

// logDiagnostic emits internal diagnostic messages, guarding against nil
// loggers in tests.
func logDiagnostic(logger *slog.Logger, level slog.Level, msg string, attrs ...slog.Attr) {
	if logger == nil {
		return
	}
	logger.LogAttrs(context.Background(), level, msg, attrs...)
}

type sourceAwareHandler struct{ slog.Handler }

// HasSource implements [slog.Handler] and signals that source metadata is
// available on the wrapped handler.
func (h sourceAwareHandler) HasSource() bool { return true }

// WithAttrs forwards attribute state while preserving source awareness.
func (h sourceAwareHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	child := h.Handler.WithAttrs(attrs)
	if child == nil {
		return nil
	}
	return sourceAwareHandler{Handler: child}
}

// WithGroup groups attributes on the wrapped handler without dropping source
// metadata hints.
func (h sourceAwareHandler) WithGroup(name string) slog.Handler {
	child := h.Handler.WithGroup(name)
	if child == nil {
		return nil
	}
	return sourceAwareHandler{Handler: child}
}
