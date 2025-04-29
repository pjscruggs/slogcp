package gcp

import (
	"context"
	"fmt"
	"runtime"

	loggingpb "cloud.google.com/go/logging/apiv2/loggingpb"
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
// (projects/PROJECT_ID/traces/TRACE_ID).
//
// If the context does not contain a valid OpenTelemetry SpanContext, or if the
// projectID is empty, it returns empty strings for traceID and spanID, and
// sampled will be false, indicating that trace information cannot be correlated.
func ExtractTraceSpan(ctx context.Context, projectID string) (traceID, spanID string, sampled bool) {
	// Retrieve the SpanContext associated with the Go context.
	sCtx := trace.SpanContextFromContext(ctx)

	// A valid SpanContext must have non-zero TraceID and SpanID.
	if !sCtx.IsValid() {
		return "", "", false
	}

	// Extract the raw components from the valid SpanContext.
	traceIDInternal := sCtx.TraceID()
	spanIDInternal := sCtx.SpanID()
	sampled = sCtx.IsSampled() // Get the sampling decision.

	// Format the traceID into the GCP required format.
	// Correlation requires both a valid TraceID from the context and a non-empty projectID.
	if traceIDInternal.IsValid() && projectID != "" {
		traceID = fmt.Sprintf("projects/%s/traces/%s", projectID, traceIDInternal.String())
	} else {
		// If traceID is invalid or projectID is missing, clear all trace info
		// as correlation is not possible without both.
		return "", "", false
	}

	// Format the spanID if it's valid.
	if spanIDInternal.IsValid() {
		spanID = spanIDInternal.String()
	} else {
		// If spanID is invalid, clear it. The sampling decision remains valid
		// as it pertains to the overall trace.
		spanID = ""
	}

	return traceID, spanID, sampled
}

// resolveSourceLocation converts a program counter (PC) value, typically obtained
// from slog.Record.PC, into source code location details (file path, line number,
// and function name).
//
// It returns a *loggingpb.LogEntrySourceLocation suitable for populating the
// corresponding field in a Cloud Logging entry. If the PC is zero or cannot be
// resolved to a valid file and function, it returns nil.
func resolveSourceLocation(pc uintptr) *loggingpb.LogEntrySourceLocation {
	// A PC of zero indicates source location was not requested or unavailable.
	if pc < 1 {
		return nil
	}
	// The PC from Record.PC is a return address; subtract 1 to land on the call site.
	pc--

	// Drive the single-PC slice through CallersFrames to handle inlining
	// and instrumentation correctly.
	frames := runtime.CallersFrames([]uintptr{pc})
	frame, _ := frames.Next()

	// If File or Function is empty, we couldn't resolve it.
	if frame.File == "" || frame.Function == "" {
		return nil
	}

	// Assemble and return the source location message.
	return &loggingpb.LogEntrySourceLocation{
		File:     frame.File,
		Line:     int64(frame.Line),
		Function: frame.Function,
	}
}
