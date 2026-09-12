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
	"log/slog"
	"time"
)

// Entry is a resolved, enriched log entry before transport encoding. Metadata
// represented by its fields is removed from Payload. Message, serviceContext,
// error information, and application attributes remain in Payload.
//
// Payload contains Go values, including custom JSON marshalers; it is not a
// JSON-normalized map. Exporters choose how to encode these values and handle
// unsupported values. The default JSON writer's encoding fallback does not run
// on the exporter path.
//
// All reference data is borrowed, read-only, and valid only during Export.
// Exporters must synchronously consume it or copy everything they retain.
type Entry struct {
	Payload map[string]any
	// Timestamp is zero when timestamp emission is disabled.
	Timestamp time.Time
	// Level preserves the original slog level, including custom levels.
	Level slog.Level
	// Severity is the full severity name (including custom-level offsets),
	// independent of severity aliases. Exporters requiring an enum can map Level.
	Severity       string
	Trace          string
	SpanID         string
	TraceSampled   bool
	Labels         map[string]string
	SourceLocation *SourceLocation
	// HTTPRequest contains the normalized Cloud Logging HTTP request fields.
	HTTPRequest map[string]any
}

// SourceLocation identifies the code that emitted a record.
type SourceLocation struct {
	File     string `json:"file"`
	Line     int64  `json:"line"`
	Function string `json:"function"`
}

// EntryExporter receives entries after attribute resolution, grouping,
// replacement, and Cloud Logging enrichment. Export may be called concurrently,
// including by handlers derived with WithAttrs and WithGroup. It must treat
// borrowed data as read-only and not retain it after returning. Background delivery requires
// the exporter to take its own snapshot before returning.
//
// An Export error is returned by Handler.Handle. Ordinary slog.Logger methods
// do not return it. Exporters with asynchronous delivery must provide their own
// error reporting and flush API. The context is the record's context; accepting
// an entry does not imply backend acknowledgement.
type EntryExporter interface {
	Export(context.Context, Entry) error
}

// NewHandlerWithExporter builds a handler that exports enriched entries instead
// of writing JSON. The exporter must be non-nil. Existing enrichment, level,
// middleware, fan-out, and explicit WithAsync options apply.
//
// Output redirects (including environment-configured file paths) are ignored;
// no output file is opened. Invalid environment configuration still returns an
// error. WithAsyncOnFile does not enable an async queue. Timestamps default to
// enabled on all runtimes; WithTime and SLOGCP_TIME override this default.
// WithSeverityAliases only affects JSON output.
// Exporters handle encoding and unsupported payload values themselves.
//
// The caller owns the exporter. Close, Shutdown, and Abort manage slogcp's own
// queue and resources only, and never close or flush the exporter. Stop logging
// and drain the handler before flushing or closing the exporter. An exporter
// that already buffers entries usually does not need WithAsync.
func NewHandlerWithExporter(exporter EntryExporter, opts ...Option) (*Handler, error) {
	if exporter == nil {
		return nil, errors.New("slogcp: nil entry exporter")
	}
	return newHandler(nil, exporter, opts...)
}

// exportEntry separates recognized metadata without serializing the payload.
// Unrecognized application values at reserved keys stay in Payload.
func (h *jsonHandler) exportEntry(r slog.Record, payload map[string]any) Entry {
	entry := Entry{
		Payload:  payload,
		Level:    r.Level,
		Severity: severityString(r.Level, false),
	}
	takeEntryField(payload, TraceKey, &entry.Trace)
	takeEntryField(payload, SpanKey, &entry.SpanID)
	takeEntryField(payload, SampledKey, &entry.TraceSampled)
	takeEntryField(payload, labelsGroupKey, &entry.Labels)
	takeEntryField(payload, "logging.googleapis.com/sourceLocation", &entry.SourceLocation)
	takeEntryField(payload, httpRequestKey, &entry.HTTPRequest)
	delete(payload, "severity")
	if h.cfg.EmitTimeField {
		entry.Timestamp = r.Time
		delete(payload, "time")
	}
	return entry
}

// takeEntryField promotes metadata only when its value has the expected type.
func takeEntryField[T any](payload map[string]any, key string, destination *T) {
	value, ok := payload[key].(T)
	if ok {
		*destination = value
		delete(payload, key)
	}
}
