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
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"cloud.google.com/go/logging"
)

// Constants defining the maximum stack frames to capture.
// Used when capturing the fallback stack trace in the handler.
const (
	maxStackFrames = 64
)

// Constants for standard keys used within the structured log payload (jsonPayload)
// and for special attribute handling.
const (
	messageKey     = "message"     // Key for the main log message string.
	errorKey       = "error"       // Default key for the *first* formatted error object.
	errorTypeKey   = "type"        // Key within the error object for the Go error type.
	stackTraceKey  = "stack_trace" // Key for the formatted stack trace string (for GCER).
	httpRequestKey = "httpRequest" // Special key used to pass HTTPRequest struct to the handler.
)

// formattedError holds the processed error details for structured logging,
// intended for consumption by Cloud Error Reporting. The handler adds these
// fields alongside the stack_trace field to the main payload.
type formattedError struct {
	Message string `json:"message"` // Error message string (from err.Error()).
	Type    string `json:"type"`    // Go type of the original error (e.g., "*errors.errorString").
}

// formatErrorForReporting prepares an error for Cloud Error Reporting by extracting
// its type and message. It also attempts to extract and format an *origin* stack trace
// if the error provides one via the stackTracer interface.
//
// It returns the basic formattedError struct (containing Type and Message) and
// the formatted origin stack trace string separately. The stack trace string will be
// empty if the error did not provide one via the interface. The decision to capture
// a *fallback* stack trace is made by the caller (the handler).
func formatErrorForReporting(err error) (fe formattedError, originStackTrace string) {

	if err == nil {
		fe = formattedError{Message: "<nil error>", Type: ""}
		return // Return empty struct and empty stack trace for nil error.
	}

	// Basic information available for all errors.
	fe = formattedError{
		Message: err.Error(),
		Type:    fmt.Sprintf("%T", err),
	}

	// Delegate origin stack trace extraction and formatting to the dedicated function.
	originStackTrace = extractAndFormatOriginStack(err)

	return fe, originStackTrace
}

// resolveSlogValue converts an slog.Value into a Go type suitable for JSON
// marshalling within the Cloud Logging entry's payload.
//
// It handles standard slog kinds, recursively resolves groups, and calls
// LogValue() on slog.LogValuer implementations. Special types like error
// and *logging.HTTPRequest are returned as-is for specific handling by the caller
// (the gcpHandler.Handle method). It returns nil for empty groups or nil values.
func resolveSlogValue(v slog.Value) any {
	// Resolve LogValuer implementations first. This allows types to customize
	// their representation before standard kind handling.
	// v.Resolve() handles potential cycles and depth limits internally.
	rv := v.Resolve() // rv is guaranteed not to be KindLogValuer.

	switch rv.Kind() {
	case slog.KindGroup:
		groupAttrs := rv.Group()
		// Return nil for empty groups to avoid empty {} in JSON.
		if len(groupAttrs) == 0 {
			return nil
		}
		// Recursively resolve attributes within the group.
		groupMap := make(map[string]any, len(groupAttrs))
		for _, ga := range groupAttrs {
			// Skip attributes with empty keys within groups.
			if ga.Key == "" {
				continue
			}
			resolvedGroupVal := resolveSlogValue(ga.Value)
			// Only add non-nil resolved values to the map.
			if resolvedGroupVal != nil {
				groupMap[ga.Key] = resolvedGroupVal
			}
		}
		// Return nil if the group becomes empty after resolving its attributes.
		if len(groupMap) == 0 {
			return nil
		}
		return groupMap // Return the map representing the nested JSON object.
	case slog.KindBool:
		return rv.Bool()
	case slog.KindDuration:
		// Represent duration as a string for human readability in logs.
		return rv.Duration().String()
	case slog.KindFloat64:
		return rv.Float64()
	case slog.KindInt64:
		return rv.Int64()
	case slog.KindString:
		return rv.String()
	case slog.KindTime:
		// Format time in RFC3339Nano with UTC for standard representation.
		// GCP Cloud Logging expects timestamps in this format.
		return rv.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindUint64:
		return rv.Uint64()
	case slog.KindAny:
		fallthrough // Handle KindAny and default together.
	default:
		// Handle arbitrary underlying types.
		val := rv.Any()

		// Check for special types that need specific handling by the caller (gcpHandler).
		// We return them directly so the handler can process them appropriately
		// (e.g., format errors, extract HTTPRequest).
		switch v := val.(type) {
		case error:
			return v // Return raw error for handler to format.
		case *logging.HTTPRequest:
			return v // Return raw HTTPRequest struct pointer for handler.
		case *http.Request:
			// Avoid logging the raw *http.Request in the general payload.
			// Relevant info should be in the *logging.HTTPRequest.
			return nil
		}

		// For other 'any' types, return the underlying value directly.
		// It's the responsibility of the caller to ensure these types are
		// suitable for JSON marshalling by the underlying logging client library.
		// Return nil if the underlying value is nil itself.
		if val == nil {
			return nil
		}
		return val
	}
}
