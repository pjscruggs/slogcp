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
	"log/slog"
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

	cfg := errorReportingConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	if cfg.service == "" {
		if info := DetectRuntimeInfo(); len(info.ServiceContext) > 0 {
			if v, ok := info.ServiceContext["service"]; ok {
				cfg.service = v
			}
			if cfg.version == "" {
				if v, ok := info.ServiceContext["version"]; ok {
					cfg.version = v
				}
			}
		}
	}

	stack, firstFrame := CaptureStack(nil)

	attrs := make([]slog.Attr, 0, 5)
	if cfg.service != "" {
		sc := map[string]any{"service": cfg.service}
		if cfg.version != "" {
			sc["version"] = cfg.version
		}
		attrs = append(attrs, slog.Any("serviceContext", sc))
	}
	if stack != "" {
		attrs = append(attrs, slog.String("stack_trace", stack))
	}
	if firstFrame.Function != "" || firstFrame.File != "" || firstFrame.Line != 0 {
		reportLocation := map[string]any{}
		if firstFrame.File != "" {
			reportLocation["filePath"] = firstFrame.File
		}
		if firstFrame.Line != 0 {
			reportLocation["lineNumber"] = firstFrame.Line
		}
		if firstFrame.Function != "" {
			reportLocation["functionName"] = firstFrame.Function
		}
		if len(reportLocation) > 0 {
			attrs = append(attrs, slog.Any("context", map[string]any{
				"reportLocation": reportLocation,
			}))
		}
	}
	if cfg.message != "" {
		attrs = append(attrs, slog.String("message", cfg.message))
	}
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
