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
	"sync"

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

var (
	traceProjectEnvOnce sync.Once
	traceProjectEnvID   string
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
// emitting per-request loggers. When a project ID is known this helper emits the
// Cloud Logging correlation fields (`logging.googleapis.com/trace`,
// `logging.googleapis.com/spanId`, and `logging.googleapis.com/trace_sampled`).
// When no project ID can be determined it falls back to OpenTelemetry-style
// keys (`otel.trace_id`, `otel.span_id`, and `otel.trace_sampled`).
//
// When projectID is empty, the helper falls back to environment variables in
// the following order: SLOGCP_TRACE_PROJECT_ID, SLOGCP_PROJECT_ID, and
// GOOGLE_CLOUD_PROJECT.
func TraceAttributes(ctx context.Context, projectID string) ([]slog.Attr, bool) {
	if ctx == nil {
		return nil, false
	}

	projectID = resolveTraceProject(projectID)

	fmtTrace, rawTrace, rawSpan, sampled, sc := ExtractTraceSpan(ctx, projectID)
	if !sc.IsValid() {
		return nil, false
	}

	attrs := buildTraceAttrSet(fmtTrace, rawTrace, rawSpan, sampled, !sc.IsRemote())
	return attrs, true
}

// resolveTraceProject chooses a project ID from input or cached environment.
func resolveTraceProject(projectID string) string {
	if normalized, _, ok := normalizeTraceProjectID(projectID); ok {
		return normalized
	}
	return cachedTraceProjectID()
}

// buildTraceAttrSet chooses Cloud Trace or OTel style attributes based on inputs.
func buildTraceAttrSet(fmtTrace, rawTrace, rawSpan string, sampled bool, ownsSpan bool) []slog.Attr {
	if fmtTrace != "" {
		return buildCloudTraceAttrs(fmtTrace, rawSpan, sampled, ownsSpan)
	}
	return buildOTelTraceAttrs(rawTrace, rawSpan, sampled, ownsSpan)
}

// buildCloudTraceAttrs emits Cloud Logging trace correlation attributes.
func buildCloudTraceAttrs(fmtTrace, rawSpan string, sampled bool, ownsSpan bool) []slog.Attr {
	attrs := []slog.Attr{slog.String(TraceKey, fmtTrace), slog.Bool(SampledKey, sampled)}
	if ownsSpan && rawSpan != "" {
		attrs = append(attrs, slog.String(SpanKey, rawSpan))
	}
	return attrs
}

// buildOTelTraceAttrs emits OpenTelemetry trace identifiers when a project is unknown.
func buildOTelTraceAttrs(rawTrace, rawSpan string, sampled bool, ownsSpan bool) []slog.Attr {
	attrs := make([]slog.Attr, 0, 3)
	if rawTrace != "" {
		attrs = append(attrs, slog.String("otel.trace_id", rawTrace))
	}
	if ownsSpan && rawSpan != "" {
		attrs = append(attrs, slog.String("otel.span_id", rawSpan))
	}
	attrs = append(attrs, slog.Bool("otel.trace_sampled", sampled))
	return attrs
}

// cachedTraceProjectID returns the Cloud project ID inferred from environment
// variables, computing the value at most once per process.
func cachedTraceProjectID() string {
	traceProjectEnvOnce.Do(func() {
		traceProjectEnvID = detectTraceProjectIDFromEnv()
	})
	return traceProjectEnvID
}

// detectTraceProjectIDFromEnv inspects known environment variables in
// priority order and returns the first valid project identifier.
func detectTraceProjectIDFromEnv() string {
	candidates := []string{
		"SLOGCP_TRACE_PROJECT_ID",
		"SLOGCP_PROJECT_ID",
		"SLOGCP_GCP_PROJECT",
		"GOOGLE_CLOUD_PROJECT",
		"GCLOUD_PROJECT",
		"GCP_PROJECT",
	}
	for _, name := range candidates {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			continue
		}
		if normalized, _, ok := normalizeTraceProjectID(value); ok {
			return normalized
		}
	}
	return ""
}

// normalizeTraceProjectID normalizes and validates a trace project identifier.
func normalizeTraceProjectID(raw string) (normalized string, changed bool, ok bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false, false
	}
	if trimmed != raw {
		changed = true
	}

	s := trimmed
	if strings.HasPrefix(strings.ToLower(s), "projects/") {
		s = s[len("projects/"):]
		changed = true
	}
	s = strings.TrimSpace(s)
	if s != trimmed {
		changed = true
	}
	if strings.Contains(s, "/") {
		return "", false, false
	}

	lower := strings.ToLower(s)
	if lower != s {
		changed = true
	}
	if !projectIDPattern.MatchString(lower) {
		return "", false, false
	}
	return lower, changed, true
}
