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
	"log"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pjscruggs/slogcp/slogcpasync"
)

const (
	// LabelsGroup is the Cloud Logging attribute group used for structured labels.
	LabelsGroup = "logging.googleapis.com/labels"

	envLogLevel         = "SLOGCP_LEVEL"
	envLogSource        = "SLOGCP_SOURCE_LOCATION"
	envLogTime          = "SLOGCP_TIME"
	envLogStackEnabled  = "SLOGCP_STACK_TRACE_ENABLED"
	envLogStackLevel    = "SLOGCP_STACK_TRACE_LEVEL"
	envSeverityAliases  = "SLOGCP_SEVERITY_ALIASES"
	envTraceProjectID   = "SLOGCP_TRACE_PROJECT_ID"
	envProjectID        = "SLOGCP_PROJECT_ID"
	envGoogleProject    = "GOOGLE_CLOUD_PROJECT"
	envTarget           = "SLOGCP_TARGET"
	envSlogcpGCP        = "SLOGCP_GCP_PROJECT"
	envTraceDiagnostics = "SLOGCP_TRACE_DIAGNOSTICS"
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

var (
	// ErrInvalidRedirectTarget indicates an unsupported value for SLOGCP_TARGET or redirect options.
	ErrInvalidRedirectTarget = errors.New("slogcp: invalid redirect target")

	handlerEnvConfigCache atomic.Pointer[handlerConfig]
)

// Option mutates Handler construction behaviour when supplied to [NewHandler].
//
// Options follow the functional options pattern and are applied in the order
// they are provided by the caller.
type Option func(*options)

// Middleware adapts a [slog.Handler] before it is exposed by [Handler].
// Middleware functions run in the order they are supplied, wrapping the core
// handler from last to first to mirror idiomatic HTTP middleware composition.
type Middleware func(slog.Handler) slog.Handler

// Handler routes slog records to Google Cloud Logging with optional
// middlewares, stack traces and trace correlation.
type Handler struct {
	slog.Handler

	cfg              *handlerConfig
	internalLogger   *slog.Logger
	switchableWriter *SwitchableWriter
	ownedFile        *os.File
	levelVar         *slog.LevelVar

	mu        sync.Mutex
	closeOnce sync.Once
}

type diagLogger interface {
	Printf(format string, args ...any)
}

var traceDiagnosticLogger diagLogger = log.New(os.Stderr, "slogcp: ", log.LstdFlags)

type traceDiagnostics struct {
	mode      TraceDiagnosticsMode
	logger    diagLogger
	unknownMu sync.Once
}

// newTraceDiagnostics constructs a traceDiagnostics helper unless tracing is disabled.
func newTraceDiagnostics(mode TraceDiagnosticsMode) *traceDiagnostics {
	if mode == TraceDiagnosticsOff {
		return nil
	}
	logger := traceDiagnosticLogger
	if logger == nil {
		logger = log.New(io.Discard, "slogcp: ", log.LstdFlags)
	}
	return &traceDiagnostics{mode: mode, logger: logger}
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

type handlerConfig struct {
	Level                 slog.Level
	AddSource             bool
	EmitTimeField         bool
	StackTraceEnabled     bool
	StackTraceLevel       slog.Level
	TraceProjectID        string
	TraceDiagnostics      TraceDiagnosticsMode
	UseShortSeverityNames bool
	Writer                io.Writer
	ClosableWriter        io.Closer
	writerExternallyOwned bool
	FilePath              string
	ReplaceAttr           func([]string, slog.Attr) slog.Attr
	Middlewares           []Middleware
	InitialAttrs          []slog.Attr
	InitialGroupedAttrs   []groupedAttr
	InitialGroups         []string

	runtimeLabels            map[string]string
	runtimeServiceContext    map[string]string
	runtimeServiceContextAny map[string]any
	traceAllowAutoformat     bool
	traceDiagnosticsState    *traceDiagnostics
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
}

// NewHandler builds a Google Cloud aware slog [Handler]. It inspects the
// environment for configuration overrides and then applies any provided
// [Option] values. The handler writes to defaultWriter unless a redirect
// option or environment override is provided.
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
	builder := &options{}
	for _, opt := range opts {
		if opt != nil {
			opt(builder)
		}
	}

	internalLogger := builder.internalLogger
	if internalLogger == nil {
		internalLogger = slog.New(slog.DiscardHandler)
	}

	cfg, err := cachedConfigFromEnv(internalLogger)
	if err != nil {
		return nil, err
	}

	applyOptions(&cfg, builder)

	ensureWriterDefaults(&cfg, defaultWriter)

	var (
		ownedFile    *os.File
		switchWriter *SwitchableWriter
	)

	if cfg.FilePath != "" {
		file, err := os.OpenFile(cfg.FilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("slogcp: open log file %q: %w", cfg.FilePath, err)
		}
		ownedFile = file
		switchWriter = NewSwitchableWriter(file)
		cfg.Writer = switchWriter
		cfg.ClosableWriter = nil
		cfg.writerExternallyOwned = false
	}

	ensureWriterFallback(&cfg)

	if cfg.ClosableWriter == nil && !cfg.writerExternallyOwned {
		if c, ok := cfg.Writer.(io.Closer); ok && !isStdStream(cfg.Writer) {
			cfg.ClosableWriter = c
		}
	}

	levelVar := builder.levelVar
	if levelVar == nil {
		levelVar = new(slog.LevelVar)
	}
	levelVar.Set(cfg.Level)

	runtimeInfo := DetectRuntimeInfo()
	cfg.runtimeLabels = cloneStringMap(runtimeInfo.Labels)
	cfg.runtimeServiceContext = cloneStringMap(runtimeInfo.ServiceContext)
	cfg.runtimeServiceContextAny = stringMapToAny(cfg.runtimeServiceContext)
	if cfg.TraceProjectID == "" {
		cfg.TraceProjectID = strings.TrimSpace(runtimeInfo.ProjectID)
	}
	cfg.traceAllowAutoformat = cfg.TraceProjectID == "" && prefersManagedGCPDefaults(runtimeInfo)
	cfg.traceDiagnosticsState = newTraceDiagnostics(cfg.TraceDiagnostics)
	if cfg.TraceDiagnostics == TraceDiagnosticsStrict && cfg.TraceProjectID == "" && !cfg.traceAllowAutoformat {
		return nil, errors.New("slogcp: trace diagnostics strict mode requires a Cloud project ID; set SLOGCP_TRACE_PROJECT_ID or provide slogcp.WithTraceProjectID")
	}

	cfgPtr := &cfg
	core := newJSONHandler(cfgPtr, levelVar, internalLogger)
	handler := slog.Handler(core)
	for i := len(cfgPtr.Middlewares) - 1; i >= 0; i-- {
		handler = cfgPtr.Middlewares[i](handler)
	}
	if builder.asyncEnabled && (!builder.asyncOnFileTargets || cfgPtr.FilePath != "") {
		handler = slogcpasync.Wrap(handler, builder.asyncOpts...)
	}
	if cfgPtr.AddSource {
		handler = sourceAwareHandler{Handler: handler}
	}

	h := &Handler{
		Handler:          handler,
		cfg:              cfgPtr,
		internalLogger:   internalLogger,
		switchableWriter: switchWriter,
		ownedFile:        ownedFile,
		levelVar:         levelVar,
	}

	return h, nil
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
// writer implementations created by options. It is safe to call multiple
// times; only the first invocation performs work.
func (h *Handler) Close() error {
	var firstErr error
	h.closeOnce.Do(func() {
		h.mu.Lock()
		closeOwnedFile := h.ownedFile != nil
		if h.switchableWriter != nil {
			if err := h.switchableWriter.Close(); err != nil {
				if firstErr == nil {
					firstErr = err
					h.internalLogger.Error("failed to close switchable writer", slog.Any("error", err))
				}
			} else {
				closeOwnedFile = false
			}
			h.switchableWriter = nil
		}
		if closeOwnedFile && h.ownedFile != nil {
			if err := h.ownedFile.Close(); err != nil && firstErr == nil {
				firstErr = err
				h.internalLogger.Error("failed to close log file", slog.Any("error", err))
			}
		}
		h.ownedFile = nil
		h.mu.Unlock()

		if h.cfg != nil && h.cfg.ClosableWriter != nil {
			if err := h.cfg.ClosableWriter.Close(); err != nil && firstErr == nil {
				firstErr = err
				h.internalLogger.Error("failed to close writer", slog.Any("error", err))
			}
		}
	})
	return firstErr
}

// ReopenLogFile rotates the handler's file writer when logging to a file.
// If the handler is not writing to a file the method is a no-op.
func (h *Handler) ReopenLogFile() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.cfg == nil || h.cfg.FilePath == "" || h.switchableWriter == nil {
		return nil
	}

	if h.ownedFile != nil {
		if err := h.ownedFile.Close(); err != nil {
			h.internalLogger.Warn("error closing log file before reopen", slog.Any("error", err))
		}
	}

	file, err := os.OpenFile(h.cfg.FilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("slogcp: reopen log file %q: %w", h.cfg.FilePath, err)
	}

	h.ownedFile = file
	h.switchableWriter.SetWriter(file)
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
// state in sync. When supplied, the handler inherits the LevelVar's current
// value after other options and environment overrides have been applied.
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
// omits timestamps on managed GCP runtimes such as Cloud Run or App Engine,
// since Cloud Logging stamps entries automatically. Enabling this option stops
// slogcp from suppressing `log/slog`'s default timestamp behavior.
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

// WithAsync wraps the constructed handler in slogcpasync using tuned per-mode defaults.
// Supply slogcpasync options to override queue, workers, drop mode, or batch size.
func WithAsync(opts ...slogcpasync.Option) Option {
	return func(o *options) {
		o.asyncEnabled = true
		o.asyncOnFileTargets = false
		o.asyncOpts = append(o.asyncOpts, opts...)
	}
}

// WithAsyncOnFileTargets wraps the handler in slogcpasync only when logging to a file.
// This keeps stdout/stderr handlers synchronous while file targets gain async defaults.
func WithAsyncOnFileTargets(opts ...slogcpasync.Option) Option {
	return func(o *options) {
		o.asyncEnabled = true
		o.asyncOnFileTargets = true
		o.asyncOpts = append(o.asyncOpts, opts...)
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

var (
	loadConfigFromEnvFunc atomic.Value // func(*slog.Logger) (handlerConfig, error)
	cachedConfigRaceHook  atomic.Value // func()
)

// init seeds the handler configuration loader defaults.
func init() {
	setLoadConfigFromEnv(loadConfigFromEnv)
}

// setLoadConfigFromEnv overrides the function used to load handler configuration from environment variables.
func setLoadConfigFromEnv(fn func(*slog.Logger) (handlerConfig, error)) {
	if fn == nil {
		fn = loadConfigFromEnv
	}
	loadConfigFromEnvFunc.Store(fn)
}

// getLoadConfigFromEnv returns the current handler configuration loader, defaulting to the built-in implementation.
func getLoadConfigFromEnv() func(*slog.Logger) (handlerConfig, error) {
	if fn, ok := loadConfigFromEnvFunc.Load().(func(*slog.Logger) (handlerConfig, error)); ok && fn != nil {
		return fn
	}
	return loadConfigFromEnv
}

// setCachedConfigRaceHook installs a hook invoked when cachedConfigFromEnv observes a concurrent cache fill.
func setCachedConfigRaceHook(fn func()) {
	cachedConfigRaceHook.Store(fn)
}

// getCachedConfigRaceHook returns the currently configured cache race hook, if any.
func getCachedConfigRaceHook() func() {
	if fn, ok := cachedConfigRaceHook.Load().(func()); ok {
		return fn
	}
	return nil
}

// loadConfigFromEnv reads handler configuration overrides from environment
// variables, logging validation issues to logger.
func cachedConfigFromEnv(logger *slog.Logger) (handlerConfig, error) {
	if cached := handlerEnvConfigCache.Load(); cached != nil {
		return *cached, nil
	}

	cfg, err := getLoadConfigFromEnv()(logger)
	if err != nil {
		return handlerConfig{}, err
	}

	entry := new(handlerConfig)
	*entry = cfg
	if handlerEnvConfigCache.CompareAndSwap(nil, entry) {
		return cfg, nil
	}
	if hook := getCachedConfigRaceHook(); hook != nil {
		hook()
	}
	if cached := handlerEnvConfigCache.Load(); cached != nil {
		return *cached, nil
	}
	return cfg, nil
}

// resetHandlerConfigCache clears the cached handler configuration derived from
// environment variables, forcing the next handler to re-read the environment.
func resetHandlerConfigCache() {
	handlerEnvConfigCache.Store(nil)
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
	}

	cfg.Level = parseLevelEnv(os.Getenv(envLogLevel), cfg.Level, logger)
	cfg.AddSource = parseBoolEnv(os.Getenv(envLogSource), cfg.AddSource, logger)
	cfg.EmitTimeField = parseBoolEnv(os.Getenv(envLogTime), cfg.EmitTimeField, logger)
	cfg.StackTraceEnabled = parseBoolEnv(os.Getenv(envLogStackEnabled), cfg.StackTraceEnabled, logger)
	cfg.StackTraceLevel = parseLevelEnv(os.Getenv(envLogStackLevel), cfg.StackTraceLevel, logger)
	cfg.UseShortSeverityNames = parseBoolEnv(os.Getenv(envSeverityAliases), cfg.UseShortSeverityNames, logger)
	cfg.TraceDiagnostics = parseTraceDiagnosticsEnv(os.Getenv(envTraceDiagnostics), cfg.TraceDiagnostics, logger)

	traceProject := strings.TrimSpace(os.Getenv(envTraceProjectID))
	if traceProject == "" {
		traceProject = strings.TrimSpace(os.Getenv(envProjectID))
	}
	if traceProject == "" {
		traceProject = strings.TrimSpace(os.Getenv(envSlogcpGCP))
	}
	if traceProject == "" {
		traceProject = strings.TrimSpace(os.Getenv(envGoogleProject))
	}
	cfg.TraceProjectID = traceProject

	if err := applyTargetFromEnv(&cfg, logger); err != nil {
		return handlerConfig{}, err
	}

	return cfg, nil
}

// applyOptions merges user-supplied options into the derived handler
// configuration.
func applyOptions(cfg *handlerConfig, o *options) {
	if o.level != nil {
		cfg.Level = *o.level
	}
	if o.addSource != nil {
		cfg.AddSource = *o.addSource
	}
	if o.emitTimeField != nil {
		cfg.EmitTimeField = *o.emitTimeField
	}
	if o.stackTraceEnabled != nil {
		cfg.StackTraceEnabled = *o.stackTraceEnabled
	}
	if o.stackTraceLevel != nil {
		cfg.StackTraceLevel = *o.stackTraceLevel
	}
	if o.levelVar != nil {
		cfg.Level = o.levelVar.Level()
	}
	if o.traceProjectID != nil {
		cfg.TraceProjectID = *o.traceProjectID
	}
	if o.traceDiagnostics != nil {
		cfg.TraceDiagnostics = *o.traceDiagnostics
	}
	if o.useShortSeverity != nil {
		cfg.UseShortSeverityNames = *o.useShortSeverity
	}
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
	if strings.TrimSpace(value) == "" {
		return current
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		logDiagnostic(logger, slog.LevelWarn, "invalid boolean environment variable", slog.String("value", value), slog.Any("error", err))
		return current
	}
	return b
}

// parseLevelEnv parses slog levels from environment variables, retaining the
// current level on failure.
func parseLevelEnv(value string, current slog.Level, logger *slog.Logger) slog.Level {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		return current
	}

	switch trimmed {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "default":
		return slog.Level(LevelDefault)
	case "notice":
		return slog.Level(LevelNotice)
	case "critical":
		return slog.Level(LevelCritical)
	case "alert":
		return slog.Level(LevelAlert)
	case "emergency":
		return slog.Level(LevelEmergency)
	default:
		if lv, err := strconv.Atoi(trimmed); err == nil {
			return slog.Level(lv)
		}
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
