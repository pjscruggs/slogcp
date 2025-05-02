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
	"log/slog"
	"time"

	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"

	"github.com/pjscruggs/slogcp/internal/gcp"
)

// LogTarget defines the destination for log output.
// It is an alias for the internal gcp.LogTarget type.
type LogTarget = gcp.LogTarget

const (
	// LogTargetGCP directs logs to the Google Cloud Logging API (default).
	LogTargetGCP LogTarget = gcp.LogTargetGCP
	// LogTargetStdout directs logs to standard output as structured JSON.
	LogTargetStdout LogTarget = gcp.LogTargetStdout
	// LogTargetStderr directs logs to standard error as structured JSON.
	LogTargetStderr LogTarget = gcp.LogTargetStderr
)

// Option configures a Logger during initialization via the New function.
// Options are applied sequentially, allowing later options to override earlier ones
// or settings derived from environment variables.
type Option func(*options)

// options holds the configurable settings for the Logger.
// This is an internal struct used by the functional options pattern.
// Fields are pointers to allow differentiating between an explicitly set
// zero value and an unset option (which would then fall back to environment
// variables or defaults).
type options struct {
	level                        *slog.Level
	addSource                    *bool
	stackTraceEnabled            *bool
	stackTraceLevel              *slog.Level
	initialAttrs                 []slog.Attr
	initialGroup                 string
	entryCountThreshold          *int
	delayThreshold               *time.Duration
	projectID                    *string
	monitoredResource            *mrpb.MonitoredResource
	addCloudRunPayloadAttributes *bool
	logTarget                    *LogTarget
}

// WithLogTarget returns an Option that explicitly sets the logging destination.
// This overrides the SLOGCP_LOG_TARGET environment variable and the default
// behavior (LogTargetGCP). Use LogTargetStdout or LogTargetStderr to bypass
// GCP client initialization and log structured JSON locally.
func WithLogTarget(target LogTarget) Option {
	return func(o *options) {
		t := target
		o.logTarget = &t
	}
}

// WithProjectID returns an Option that explicitly sets the Google Cloud Project ID
// to be used by the logger, primarily for formatting trace IDs.
// This setting takes precedence over the GOOGLE_CLOUD_PROJECT environment variable
// and automatic detection via the metadata server.
func WithProjectID(id string) Option {
	return func(o *options) {
		o.projectID = &id
	}
}

// WithMonitoredResource returns an Option that explicitly sets the MonitoredResource
// associated with log entries sent by this logger when using the GCP target.
// This overrides the automatic resource detection performed by the underlying
// `cloud.google.com/go/logging` client library. Use this if auto-detection is
// insufficient or incorrect for your environment. This option has no effect
// if the log target is Stdout or Stderr.
func WithMonitoredResource(res *mrpb.MonitoredResource) Option {
	return func(o *options) {
		o.monitoredResource = res
	}
}

// WithLevel returns an Option that sets the minimum logging level.
// This setting overrides the LOG_LEVEL environment variable.
// Use standard slog.Level constants (e.g., slog.LevelDebug)
// or slogcp.Level constants cast to slog.Level (e.g., slog.Level(slogcp.LevelNotice)).
func WithLevel(level slog.Level) Option {
	return func(o *options) {
		lvl := level
		o.level = &lvl
	}
}

// WithSourceLocationEnabled returns an Option that enables or disables
// including source code location (file, line, function) in log entries.
// This setting overrides the LOG_SOURCE_LOCATION environment variable.
// Enabling source location incurs a performance cost. Defaults to false.
func WithSourceLocationEnabled(enabled bool) Option {
	return func(o *options) {
		src := enabled
		o.addSource = &src
	}
}

// WithStackTraceEnabled returns an Option that enables or disables the automatic
// capture and inclusion of stack traces for logs at or above the configured
// stack trace level (see WithStackTraceLevel).
// This setting overrides the LOG_STACK_TRACE_ENABLED environment variable.
// Enabling stack traces incurs a performance cost. Defaults to false.
// This option only affects the GCP handler; the fallback JSON handler does not
// automatically add stack traces.
func WithStackTraceEnabled(enabled bool) Option {
	return func(o *options) {
		st := enabled
		o.stackTraceEnabled = &st
	}
}

// WithStackTraceLevel returns an Option that sets the minimum slog.Level at which
// stack traces should be captured and included in log entries, provided stack
// trace generation is enabled (see WithStackTraceEnabled).
// This setting overrides the LOG_STACK_TRACE_LEVEL environment variable.
// Defaults to slog.LevelError.
// This option only affects the GCP handler; the fallback JSON handler does not
// automatically add stack traces.
func WithStackTraceLevel(level slog.Level) Option {
	return func(o *options) {
		lvl := level
		o.stackTraceLevel = &lvl
	}
}

// WithAttrs returns an Option that adds the given attributes to the logger
// when it is created. This is equivalent to calling `logger.With(attrs...)`
// immediately after logger creation. Multiple `WithAttrs` options are cumulative.
// The provided slice is copied to prevent modification by the caller.
func WithAttrs(attrs []slog.Attr) Option {
	return func(o *options) {
		if len(attrs) == 0 {
			return // No attributes to add.
		}
		attrsCopy := make([]slog.Attr, len(attrs))
		copy(attrsCopy, attrs)
		o.initialAttrs = append(o.initialAttrs, attrsCopy...)
	}
}

// WithGroup returns an Option that adds a group namespace to the logger
// when it is created. This is equivalent to calling `logger.WithGroup(name)`
// immediately after logger creation (and after any `WithAttrs` are applied).
// If multiple `WithGroup` options are provided, only the *last* one takes effect.
func WithGroup(name string) Option {
	return func(o *options) {
		o.initialGroup = name
	}
}

// WithEntryCountThreshold returns an Option that sets the maximum number of log entries
// to buffer before sending them to the Cloud Logging API when using the GCP target.
// This overrides the default value used by the underlying `cloud.google.com/go/logging` client.
// See https://pkg.go.dev/cloud.google.com/go/logging#EntryCountThreshold for details.
// This option has no effect if the log target is Stdout or Stderr.
func WithEntryCountThreshold(count int) Option {
	return func(o *options) {
		c := count
		o.entryCountThreshold = &c
	}
}

// WithDelayThreshold returns an Option that sets the maximum time log entries are
// buffered before sending them to the Cloud Logging API when using the GCP target.
// This overrides the default value used by the underlying `cloud.google.com/go/logging` client.
// See https://pkg.go.dev/cloud.google.com/go/logging#DelayThreshold for details.
// This option has no effect if the log target is Stdout or Stderr.
func WithDelayThreshold(delay time.Duration) Option {
	return func(o *options) {
		d := delay
		o.delayThreshold = &d
	}
}

// WithCloudRunPayloadAttributes returns an Option that enables or disables adding
// Cloud Run service and revision names as attributes within the log payload when
// using the GCP target.
// This is distinct from the MonitoredResource association. Defaults to false.
// This option has no effect if the log target is Stdout or Stderr.
func WithCloudRunPayloadAttributes(enabled bool) Option {
	return func(o *options) {
		crpa := enabled
		o.addCloudRunPayloadAttributes = &crpa
	}
}
