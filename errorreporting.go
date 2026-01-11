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
	"log/slog"
	"runtime"
	"strings"
)

// ErrorReportingOption configures ErrorReportingAttrs and ReportError.
type ErrorReportingOption func(*errorReportingConfig)

type errorReportingConfig struct {
	message string
	service string
	version string
}

// WithErrorMessage overrides the message field embedded in the structured payload.
func WithErrorMessage(msg string) ErrorReportingOption {
	return func(cfg *errorReportingConfig) {
		cfg.message = msg
	}
}

// WithErrorServiceContext sets the service.name and version used for Cloud
// Error Reporting. When omitted slogcp attempts to infer these values from
// the execution environment (Cloud Run, Cloud Functions, etc).
func WithErrorServiceContext(service, version string) ErrorReportingOption {
	return func(cfg *errorReportingConfig) {
		cfg.service = strings.TrimSpace(service)
		cfg.version = strings.TrimSpace(version)
	}
}

// ErrorReportingAttrs returns structured attributes that align with the Cloud
// Error Reporting ingestion format. The caller can append the returned attrs
// to any slog logging call.
func ErrorReportingAttrs(err error, opts ...ErrorReportingOption) []slog.Attr {
	if err == nil {
		return nil
	}

	cfg := buildErrorReportingConfig(opts)
	stack, firstFrame := CaptureStack(nil)

	attrs := make([]slog.Attr, 0, 5)
	attrs = append(attrs, buildServiceContextAttrs(cfg)...)
	attrs = append(attrs, buildStackAttrs(stack)...)
	attrs = append(attrs, buildReportLocationAttrs(firstFrame)...)
	attrs = append(attrs, buildErrorMessageAttr(cfg.message)...)
	return attrs
}

// ReportError logs err using logger at error level, automatically attaching
// Cloud Error Reporting metadata. Additional attributes can be provided via opts.
func ReportError(ctx context.Context, logger *slog.Logger, err error, msg string, opts ...ErrorReportingOption) {
	if logger == nil || err == nil {
		return
	}
	attrs := ErrorReportingAttrs(err, opts...)
	attrs = append([]slog.Attr{slog.Any("error", err)}, attrs...)
	logger.LogAttrs(ctx, slog.LevelError, msg, attrs...)
}

// buildErrorReportingConfig applies options and runtime defaults for error reporting.
func buildErrorReportingConfig(opts []ErrorReportingOption) errorReportingConfig {
	cfg := errorReportingConfig{}
	applyErrorReportingOptions(&cfg, opts)

	if hasServiceAndVersion(cfg) {
		return cfg
	}

	info := DetectRuntimeInfo()
	if len(info.ServiceContext) == 0 {
		return cfg
	}

	cfg.service = firstNonEmptyString(cfg.service, info.ServiceContext["service"])
	cfg.version = firstNonEmptyString(cfg.version, info.ServiceContext["version"])
	return cfg
}

// applyErrorReportingOptions applies provided options to the config pointer.
func applyErrorReportingOptions(cfg *errorReportingConfig, opts []ErrorReportingOption) {
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
}

// hasServiceAndVersion reports whether the config already has explicit values.
func hasServiceAndVersion(cfg errorReportingConfig) bool {
	return cfg.service != "" && cfg.version != ""
}

// firstNonEmptyString returns the first non-empty string from values.
func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// buildServiceContextAttrs constructs the serviceContext attribute when available.
func buildServiceContextAttrs(cfg errorReportingConfig) []slog.Attr {
	if cfg.service == "" {
		return nil
	}
	sc := map[string]any{"service": cfg.service}
	if cfg.version != "" {
		sc["version"] = cfg.version
	}
	return []slog.Attr{slog.Any("serviceContext", sc)}
}

// buildStackAttrs emits stack trace attributes when present.
func buildStackAttrs(stack string) []slog.Attr {
	if stack == "" {
		return nil
	}
	return []slog.Attr{slog.String("stack_trace", stack)}
}

// buildReportLocationAttrs formats the reportLocation context attribute.
func buildReportLocationAttrs(frame runtime.Frame) []slog.Attr {
	if frame.Function == "" && frame.File == "" && frame.Line == 0 {
		return nil
	}
	reportLocation := map[string]any{}
	if frame.File != "" {
		reportLocation["filePath"] = frame.File
	}
	if frame.Line != 0 {
		reportLocation["lineNumber"] = frame.Line
	}
	if frame.Function != "" {
		reportLocation["functionName"] = frame.Function
	}
	return []slog.Attr{slog.Any("context", map[string]any{"reportLocation": reportLocation})}
}

// buildErrorMessageAttr attaches a message when provided.
func buildErrorMessageAttr(msg string) []slog.Attr {
	if msg == "" {
		return nil
	}
	return []slog.Attr{slog.String("message", msg)}
}
