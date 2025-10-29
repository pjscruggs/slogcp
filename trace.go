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
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// Constants for trace-related keys used when adding trace information
// as attributes to structured log payloads. These specific keys are recognized
// by Google Cloud Logging for automatic correlation with Cloud Trace.
const (
	// TraceKey is the field name for the formatted trace name.
	// The value must be "projects/PROJECT_ID/traces/TRACE_ID".
	TraceKey = "logging.googleapis.com/trace"
	// SpanKey is the field name for the hex span ID.
	SpanKey = "logging.googleapis.com/spanId"
	// SampledKey is the field name for the boolean sampling decision.
	SampledKey = "logging.googleapis.com/trace_sampled"
)

// ExtractTraceSpan extracts OpenTelemetry trace details from ctx and, if a
// non-empty projectID is provided, formats a fully-qualified Cloud Trace name
// using "projects/{projectID}/traces/{traceID}".
//
// It returns:
//   - formattedTraceID: "projects/<projectID>/traces/<traceID>" if projectID != "", else "".
//   - rawTraceID: 32-char lowercase hex trace ID.
//   - rawSpanID:  16-char lowercase hex span ID.
//   - sampled:    whether the span context is sampled.
//   - otelCtx:    the original span context (valid==true iff trace present).
//
// This function is intentionally light-weight: it does not create spans,
// does not parse headers, and does not mutate context. Upstream middleware
// should populate the OTel span context (e.g., via OTel propagators or an
// X-Cloud-Trace-Context injector) before calling this helper.
//
// For cross-project linking, pass the desired project that owns the trace as
// projectID. If empty, only raw IDs are returned.
func ExtractTraceSpan(ctx context.Context, projectID string) (formattedTraceID, rawTraceID, rawSpanID string, sampled bool, otelCtx trace.SpanContext) {
	otelCtx = trace.SpanContextFromContext(ctx)
	if !otelCtx.IsValid() {
		return "", "", "", false, otelCtx
	}

	traceID := otelCtx.TraceID()
	spanID := otelCtx.SpanID()
	rawTraceID = traceID.String()
	rawSpanID = spanID.String()
	sampled = otelCtx.IsSampled()

	if projectID != "" {
		formattedTraceID = FormatTraceResource(projectID, rawTraceID)
	}

	return formattedTraceID, rawTraceID, rawSpanID, sampled, otelCtx
}

// FormatTraceResource returns a fully-qualified Cloud Trace resource name:
//
//	projects/<projectID>/traces/<traceID>
//
// Callers should pass a 32-char lowercase hex traceID (e.g., from OTel).
func FormatTraceResource(projectID, rawTraceID string) string {
	return fmt.Sprintf("projects/%s/traces/%s", projectID, rawTraceID)
}

// SpanIDHexToDecimal converts a 16-char hex span ID to its unsigned
// decimal representation as required by the legacy X-Cloud-Trace-Context
// header's SPAN_ID field.
//
// It returns ("", false) if the value cannot be parsed.
func SpanIDHexToDecimal(spanIDHex string) (string, bool) {
	// 64-bit span IDs as hex -> decimal
	ui, err := strconv.ParseUint(spanIDHex, 16, 64)
	if err != nil {
		return "", false
	}
	return strconv.FormatUint(ui, 10), true
}

// BuildXCloudTraceContext builds the value for the legacy X-Cloud-Trace-Context
// header from raw hex IDs and a sampled flag. The format is:
//
//	TRACE_ID[/SPAN_ID][;o=TRACE_TRUE]
//
// where SPAN_ID is decimal and TRACE_TRUE is "1" when sampled, "0" otherwise.
//
// If spanIDHex is empty or invalid, the "/SPAN_ID" portion is omitted.
func BuildXCloudTraceContext(rawTraceID, spanIDHex string, sampled bool) string {
	val := rawTraceID
	if dec, ok := SpanIDHexToDecimal(spanIDHex); ok && dec != "" {
		val = fmt.Sprintf("%s/%s", val, dec)
	}
	if sampled {
		val = fmt.Sprintf("%s;o=1", val)
	} else {
		val = fmt.Sprintf("%s;o=0", val)
	}
	return val
}

// TraceAttributes extracts Cloud Trace aware attributes from ctx. The returned
// slice can be supplied to logger.With to correlate logs with traces when
// emitting per-request loggers.
//
// When projectID is empty, the helper falls back to environment variables in
// the following order: SLOGCP_TRACE_PROJECT_ID, SLOGCP_PROJECT_ID, and
// GOOGLE_CLOUD_PROJECT.
func TraceAttributes(ctx context.Context, projectID string) ([]slog.Attr, bool) {
	if ctx == nil {
		return nil, false
	}

	if projectID == "" {
		projectID = strings.TrimSpace(os.Getenv("SLOGCP_TRACE_PROJECT_ID"))
	}
	if projectID == "" {
		projectID = strings.TrimSpace(os.Getenv("SLOGCP_PROJECT_ID"))
	}
	if projectID == "" {
		projectID = strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT"))
	}

	fmtTrace, rawTrace, rawSpan, sampled, sc := ExtractTraceSpan(ctx, projectID)
	if !sc.IsValid() {
		return nil, false
	}

	attrs := []slog.Attr{
		slog.String("trace_id", rawTrace),
		slog.String(SpanKey, rawSpan),
		slog.Bool(SampledKey, sampled),
	}
	if fmtTrace != "" {
		attrs = append(attrs, slog.String(TraceKey, fmtTrace))
	}
	return attrs, true
}
