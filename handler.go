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
)

const LabelsGroup = "logging.googleapis.com/labels"

var (
	ErrInvalidRedirectTarget = errors.New("slogcp: invalid redirect target")
	errUnsupportedTarget     = errors.New("slogcp: logging to the Cloud Logging API has been removed")
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

type handlerConfig struct {
	Level                 slog.Level
	AddSource             bool
	EmitTimeField         bool
	StackTraceEnabled     bool
	StackTraceLevel       slog.Level
	TraceProjectID        string
	Writer                io.Writer
	ClosableWriter        io.Closer
	writerExternallyOwned bool
	FilePath              string
	ReplaceAttr           func([]string, slog.Attr) slog.Attr
	Middlewares           []Middleware
	InitialAttrs          []slog.Attr
	InitialGroups         []string

	runtimeLabels            map[string]string
	runtimeServiceContext    map[string]string
	runtimeServiceContextAny map[string]any
}

type options struct {
	level                 *slog.Level
	levelVar              *slog.LevelVar
	addSource             *bool
	emitTimeField         *bool
	stackTraceEnabled     *bool
	stackTraceLevel       *slog.Level
	traceProjectID        *string
	writer                io.Writer
	writerFilePath        *string
	writerExternallyOwned bool
	replaceAttr           func([]string, slog.Attr) slog.Attr
	middlewares           []Middleware
	attrs                 [][]slog.Attr
	groups                []string
	groupsSet             bool
	internalLogger        *slog.Logger
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
		internalLogger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	cfg, err := loadConfigFromEnv(internalLogger)
	if err != nil {
		return nil, err
	}

	applyOptions(&cfg, builder)

	if cfg.Writer == nil && cfg.FilePath == "" {
		if defaultWriter != nil {
			cfg.Writer = defaultWriter
			cfg.writerExternallyOwned = true
		} else {
			cfg.Writer = os.Stdout
			cfg.writerExternallyOwned = true
		}
	}

	var (
		ownedFile    *os.File
		switchWriter *SwitchableWriter
	)

	if cfg.FilePath != "" {
		file, err := os.OpenFile(cfg.FilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("slogcp: open log file %q: %w", cfg.FilePath, err)
		}
		ownedFile = file
		switchWriter = NewSwitchableWriter(file)
		cfg.Writer = switchWriter
		cfg.ClosableWriter = nil
		cfg.writerExternallyOwned = false
	}

	if cfg.Writer == nil {
		cfg.Writer = os.Stdout
		cfg.writerExternallyOwned = true
	}

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

	cfgPtr := &cfg
	core := newJSONHandler(cfgPtr, levelVar, internalLogger)
	handler := slog.Handler(core)
	for i := len(cfgPtr.Middlewares) - 1; i >= 0; i-- {
		handler = cfgPtr.Middlewares[i](handler)
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

	file, err := os.OpenFile(h.cfg.FilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
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

// DefaultContext logs a structured message at [LevelDefault] severity while
// attaching contextual attributes from ctx. It is suitable for routing
// application level events through Cloud Logging.
func DefaultContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(ctx, LevelDefault.Level(), msg, args...)
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
// external code to adjust log levels at runtime while keeping slogcp’s internal
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

// WithTime toggles emission of the top-level RFC3339Nano "time" field. When
// disabled, Cloud Logging assigns timestamps during ingestion.
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
// if necessary.
func WithRedirectToFile(path string) Option {
	trimmed := strings.TrimSpace(path)
	return func(o *options) {
		o.writer = nil
		o.writerFilePath = &trimmed
		o.writerExternallyOwned = false
	}
}

// WithRedirectWriter uses writer for log output without taking ownership of
// its lifecycle.
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

// WithAttrs preloads static attributes to be attached to every record emitted
// by the handler.
func WithAttrs(attrs []slog.Attr) Option {
	return func(o *options) {
		if len(attrs) == 0 {
			return
		}
		dup := make([]slog.Attr, len(attrs))
		copy(dup, attrs)
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

// loadConfigFromEnv reads handler configuration overrides from environment
// variables, logging validation issues to logger.
func loadConfigFromEnv(logger *slog.Logger) (handlerConfig, error) {
	cfg := handlerConfig{
		Level:           slog.LevelInfo,
		StackTraceLevel: slog.LevelError,
	}

	cfg.Level = parseLevelEnv(os.Getenv("LOG_LEVEL"), cfg.Level, logger)
	cfg.AddSource = parseBoolEnv(os.Getenv("LOG_SOURCE_LOCATION"), cfg.AddSource, logger)
	if raw := os.Getenv("LOG_TIME"); raw != "" {
		cfg.EmitTimeField = parseBoolEnv(raw, cfg.EmitTimeField, logger)
	} else {
		cfg.EmitTimeField = parseBoolEnv(os.Getenv("LOG_TIME_FIELD_ENABLED"), cfg.EmitTimeField, logger)
	}
	cfg.StackTraceEnabled = parseBoolEnv(os.Getenv("LOG_STACK_TRACE_ENABLED"), cfg.StackTraceEnabled, logger)
	cfg.StackTraceLevel = parseLevelEnv(os.Getenv("LOG_STACK_TRACE_LEVEL"), cfg.StackTraceLevel, logger)

	traceProject := strings.TrimSpace(os.Getenv("SLOGCP_TRACE_PROJECT_ID"))
	if traceProject == "" {
		traceProject = strings.TrimSpace(os.Getenv("SLOGCP_PROJECT_ID"))
	}
	if traceProject == "" {
		traceProject = strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT"))
	}
	cfg.TraceProjectID = traceProject

	if err := applyRedirectFromEnv(&cfg, logger); err != nil {
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
	if o.groupsSet {
		cfg.InitialGroups = append([]string(nil), o.groups...)
	}
}

// applyRedirectFromEnv adjusts output destinations based on environment
// variables such as SLOGCP_REDIRECT.
func applyRedirectFromEnv(cfg *handlerConfig, logger *slog.Logger) error {
	redirect := strings.TrimSpace(os.Getenv("SLOGCP_REDIRECT_AS_JSON_TARGET"))
	if redirect != "" {
		lower := strings.ToLower(redirect)
		switch {
		case strings.HasPrefix(lower, "file:"):
			path := strings.TrimSpace(redirect[len("file:"):])
			if path == "" {
				return ErrInvalidRedirectTarget
			}
			cfg.FilePath = path
			cfg.Writer = nil
			cfg.ClosableWriter = nil
			cfg.writerExternallyOwned = false
		case strings.EqualFold(redirect, "stdout"):
			cfg.Writer = os.Stdout
			cfg.FilePath = ""
			cfg.ClosableWriter = nil
			cfg.writerExternallyOwned = true
		case strings.EqualFold(redirect, "stderr"):
			cfg.Writer = os.Stderr
			cfg.FilePath = ""
			cfg.ClosableWriter = nil
			cfg.writerExternallyOwned = true
		default:
			return ErrInvalidRedirectTarget
		}
	}

	logTarget := strings.TrimSpace(os.Getenv("SLOGCP_LOG_TARGET"))
	switch strings.ToLower(logTarget) {
	case "", "stdout":
		if cfg.Writer == nil && cfg.FilePath == "" && logTarget != "" {
			cfg.Writer = os.Stdout
			cfg.writerExternallyOwned = true
		}
	case "stderr":
		if cfg.Writer == nil && cfg.FilePath == "" {
			cfg.Writer = os.Stderr
			cfg.writerExternallyOwned = true
		}
	case "file":
		if cfg.FilePath == "" {
			return ErrInvalidRedirectTarget
		}
	case "gcp":
		return errUnsupportedTarget
	default:
		if logTarget != "" {
			logDiagnostic(logger, slog.LevelWarn, "unknown SLOGCP_LOG_TARGET", slog.String("value", logTarget))
		}
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
