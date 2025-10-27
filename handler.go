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

type Option func(*options)

type Middleware func(slog.Handler) slog.Handler

type Handler struct {
	slog.Handler

	cfg              *handlerConfig
	internalLogger   *slog.Logger
	switchableWriter *SwitchableWriter
	ownedFile        *os.File

	mu        sync.Mutex
	closeOnce sync.Once
}

type handlerConfig struct {
	Level             slog.Level
	AddSource         bool
	StackTraceEnabled bool
	StackTraceLevel   slog.Level
	TraceProjectID    string
	Writer            io.Writer
	ClosableWriter    io.Closer
	FilePath          string
	ReplaceAttr       func([]string, slog.Attr) slog.Attr
	Middlewares       []Middleware
	InitialAttrs      []slog.Attr
	InitialGroup      string

	runtimeLabels            map[string]string
	runtimeServiceContext    map[string]string
	runtimeServiceContextAny map[string]any
}

type options struct {
	level             *slog.Level
	addSource         *bool
	stackTraceEnabled *bool
	stackTraceLevel   *slog.Level
	traceProjectID    *string
	writer            io.Writer
	writerFilePath    *string
	replaceAttr       func([]string, slog.Attr) slog.Attr
	middlewares       []Middleware
	attrs             [][]slog.Attr
	group             *string
	internalLogger    *slog.Logger
}

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
		} else {
			cfg.Writer = os.Stdout
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
	}

	if cfg.Writer == nil {
		cfg.Writer = os.Stdout
	}

	if cfg.ClosableWriter == nil {
		if c, ok := cfg.Writer.(io.Closer); ok && !isStdStream(cfg.Writer) {
			cfg.ClosableWriter = c
		}
	}

	levelVar := new(slog.LevelVar)
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
	}

	return h, nil
}

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

func DefaultContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(ctx, LevelDefault.Level(), msg, args...)
}

func NoticeContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(ctx, LevelNotice.Level(), msg, args...)
}

func CriticalContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(ctx, LevelCritical.Level(), msg, args...)
}

func AlertContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(ctx, LevelAlert.Level(), msg, args...)
}

func EmergencyContext(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(ctx, LevelEmergency.Level(), msg, args...)
}

func WithInternalLogger(logger *slog.Logger) Option {
	return func(o *options) {
		o.internalLogger = logger
	}
}

func WithLevel(level slog.Level) Option {
	return func(o *options) {
		o.level = &level
	}
}

func WithSourceLocationEnabled(enabled bool) Option {
	return func(o *options) {
		o.addSource = &enabled
	}
}

func WithStackTraceEnabled(enabled bool) Option {
	return func(o *options) {
		o.stackTraceEnabled = &enabled
	}
}

func WithStackTraceLevel(level slog.Level) Option {
	return func(o *options) {
		o.stackTraceLevel = &level
	}
}

func WithTraceProjectID(id string) Option {
	trimmed := strings.TrimSpace(id)
	return func(o *options) {
		o.traceProjectID = &trimmed
	}
}

func WithRedirectToStdout() Option {
	return func(o *options) {
		o.writer = os.Stdout
		o.writerFilePath = nil
	}
}

func WithRedirectToStderr() Option {
	return func(o *options) {
		o.writer = os.Stderr
		o.writerFilePath = nil
	}
}

func WithRedirectToFile(path string) Option {
	trimmed := strings.TrimSpace(path)
	return func(o *options) {
		o.writer = nil
		o.writerFilePath = &trimmed
	}
}

func WithRedirectWriter(writer io.Writer) Option {
	return func(o *options) {
		o.writer = writer
		o.writerFilePath = nil
	}
}

func WithReplaceAttr(fn func([]string, slog.Attr) slog.Attr) Option {
	return func(o *options) {
		o.replaceAttr = fn
	}
}

func WithMiddleware(mw Middleware) Option {
	return func(o *options) {
		if mw != nil {
			o.middlewares = append(o.middlewares, mw)
		}
	}
}

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

func WithGroup(name string) Option {
	trimmed := strings.TrimSpace(name)
	return func(o *options) {
		if trimmed == "" {
			o.group = nil
			return
		}
		o.group = &trimmed
	}
}

func loadConfigFromEnv(logger *slog.Logger) (handlerConfig, error) {
	cfg := handlerConfig{
		Level:           slog.LevelInfo,
		StackTraceLevel: slog.LevelError,
	}

	cfg.Level = parseLevelEnv(os.Getenv("LOG_LEVEL"), cfg.Level, logger)
	cfg.AddSource = parseBoolEnv(os.Getenv("LOG_SOURCE_LOCATION"), cfg.AddSource, logger)
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

func applyOptions(cfg *handlerConfig, o *options) {
	if o.level != nil {
		cfg.Level = *o.level
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
	if o.traceProjectID != nil {
		cfg.TraceProjectID = *o.traceProjectID
	}
	if o.writerFilePath != nil {
		cfg.FilePath = strings.TrimSpace(*o.writerFilePath)
		cfg.Writer = nil
		cfg.ClosableWriter = nil
	}
	if o.writer != nil {
		cfg.Writer = o.writer
		cfg.FilePath = ""
		if c, ok := o.writer.(io.Closer); ok && !isStdStream(o.writer) {
			cfg.ClosableWriter = c
		} else {
			cfg.ClosableWriter = nil
		}
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
	if o.group != nil {
		cfg.InitialGroup = *o.group
	}
}

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
		case strings.EqualFold(redirect, "stdout"):
			cfg.Writer = os.Stdout
			cfg.FilePath = ""
			cfg.ClosableWriter = nil
		case strings.EqualFold(redirect, "stderr"):
			cfg.Writer = os.Stderr
			cfg.FilePath = ""
			cfg.ClosableWriter = nil
		default:
			return ErrInvalidRedirectTarget
		}
	}

	logTarget := strings.TrimSpace(os.Getenv("SLOGCP_LOG_TARGET"))
	switch strings.ToLower(logTarget) {
	case "", "stdout":
		if cfg.Writer == nil && cfg.FilePath == "" && logTarget != "" {
			cfg.Writer = os.Stdout
		}
	case "stderr":
		if cfg.Writer == nil && cfg.FilePath == "" {
			cfg.Writer = os.Stderr
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
		if lv, err := strconv.Atoi(trimmed); err == nil {
			return slog.Level(lv)
		}
	}

	logDiagnostic(logger, slog.LevelWarn, "invalid log level environment variable", slog.String("value", value))
	return current
}

func isStdStream(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || f == nil {
		return false
	}
	return f.Fd() == os.Stdout.Fd() || f.Fd() == os.Stderr.Fd()
}

func logDiagnostic(logger *slog.Logger, level slog.Level, msg string, attrs ...slog.Attr) {
	if logger == nil {
		return
	}
	logger.LogAttrs(context.Background(), level, msg, attrs...)
}

type sourceAwareHandler struct{ slog.Handler }

func (h sourceAwareHandler) HasSource() bool { return true }

func (h sourceAwareHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	child := h.Handler.WithAttrs(attrs)
	if child == nil {
		return nil
	}
	return sourceAwareHandler{Handler: child}
}

func (h sourceAwareHandler) WithGroup(name string) slog.Handler {
	child := h.Handler.WithGroup(name)
	if child == nil {
		return nil
	}
	return sourceAwareHandler{Handler: child}
}
