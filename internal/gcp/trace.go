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

package gcp

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/trace"
)

// Constants for trace-related keys used when adding trace information
// as attributes to structured log payloads. These specific keys are recognized
// by Google Cloud Logging for automatic correlation with Cloud Trace.
const (
	// TraceKey is the field name for the formatted trace ID.
	TraceKey = "logging.googleapis.com/trace"
	// SpanKey is the field name for the span ID.
	SpanKey = "logging.googleapis.com/spanId"
	// SampledKey is the field name for the trace sampling decision.
	SampledKey = "logging.googleapis.com/trace_sampled"
)

// ExtractTraceSpan extracts OpenTelemetry trace ID, span ID, and sampling status
// from the provided context.Context. It formats the trace ID using the provided
// projectID according to the format expected by Google Cloud Logging
// ("projects/PROJECT_ID/traces/TRACE_ID").
//
// It returns the formatted trace ID (if possible), the raw hex trace ID, the raw
// hex span ID, the sampling decision, and the original OpenTelemetry SpanContext.
// If the context does not contain a valid SpanContext, or if the projectID is
// empty when needed for formatting, relevant return values will be empty strings
// or false.
func ExtractTraceSpan(ctx context.Context, projectID string) (formattedTraceID, rawTraceID, rawSpanID string, sampled bool, otelCtx trace.SpanContext) {
	otelCtx = trace.SpanContextFromContext(ctx)

	// Return early if the context is invalid (no trace info).
	if !otelCtx.IsValid() {
		return "", "", "", false, otelCtx
	}

	// Extract components from the valid SpanContext.
	traceIDInternal := otelCtx.TraceID()
	spanIDInternal := otelCtx.SpanID()
	sampled = otelCtx.IsSampled()
	rawTraceID = traceIDInternal.String()
	rawSpanID = spanIDInternal.String()

	// Format the traceID into the GCP required format only if projectID is available.
	if projectID != "" {
		formattedTraceID = fmt.Sprintf("projects/%s/traces/%s", projectID, rawTraceID)
	}
	// If projectID is empty, formattedTraceID remains "", but raw IDs and sampled flag are still valid.

	return formattedTraceID, rawTraceID, rawSpanID, sampled, otelCtx
}
