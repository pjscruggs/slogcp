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
	"strconv"
	"time"

	"cloud.google.com/go/logging"
	logpb "cloud.google.com/go/logging/apiv2/loggingpb"
)

// Constants for standard keys used within the structured log payload (jsonPayload)
// and for special attribute handling.
const (
	messageKey        = "message"                          // Key for the main log message string.
	errorKey          = "error"                            // Key used in RedirectAsJSON for the error message.
	errorTypeKey      = "type"                             // Key within the GCP API payload for the Go error type.
	stackTraceKey     = "stack_trace"                      // Key for the formatted stack trace string (for GCER).
	httpRequestKey    = "httpRequest"                      // Special key used to pass HTTPRequest struct to the handler.
	operationGroupKey = "logging.googleapis.com/operation" // Canonical operation group name
)

// formattedError holds the processed error details for structured logging,
// primarily for visibility within the log entry itself.
type formattedError struct {
	Message string `json:"message"` // Error message string (from err.Error()).
	Type    string `json:"type"`    // Go type of the original error (e.g., "*errors.errorString").
}

// formatErrorForReporting prepares an error by extracting its type and message.
// It also attempts to extract an *origin* stack trace if the error provides one
// via the stackTracer interface.
// It returns the basic formattedError struct and the formatted origin stack trace string.
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

	// Delegate origin stack trace extraction and formatting.
	originStackTrace = extractAndFormatOriginStack(err)

	return fe, originStackTrace
}

// resolveSlogValue converts an slog.Value into a Go type suitable for JSON
// marshalling within the Cloud Logging entry's payload.
// It handles standard slog kinds, recursively resolves groups, and calls
// LogValue() on slog.LogValuer implementations. Special types like error
// are returned as-is for specific handling by the gcpHandler.
// It returns nil for empty groups or nil values.
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
		// (e.g., format errors). Other special types like HTTPRequest are handled
		// via attribute key interception *before* calling this function.
		switch vt := val.(type) {
		case error:
			return vt // Return raw error for handler to format.
		case *logging.HTTPRequest:
			// If this function is called with an HTTPRequest value, it means
			// the handler did not correctly intercept the httpRequestKey attribute.
			// Return nil to prevent adding the raw struct to the general payload.
			return nil
		case *http.Request:
			// Never log the raw *http.Request struct.
			return nil
		}

		// Return nil if the underlying value is nil itself.
		if val == nil {
			return nil
		}
		// Return other 'any' types directly. It's assumed these are
		// suitable for JSON marshalling by the underlying logging client library
		// or the Go JSON encoder in redirect mode.
		return val
	}
}

// flattenHTTPRequestToMap converts a *logging.HTTPRequest struct into a map
// suitable for embedding within the `httpRequest` field of a structured JSON log entry,
// following GCP's expected field names. Includes zero/false/empty values for consistency.
func flattenHTTPRequestToMap(req *logging.HTTPRequest) map[string]any {
	if req == nil {
		return nil
	}
	m := make(map[string]any)
	// Use keys expected by GCP structured logging for httpRequest
	if req.Request != nil {
		m["requestMethod"] = req.Request.Method // Empty string if not set
		if req.Request.URL != nil {
			m["requestUrl"] = req.Request.URL.String() // Empty string if not set
		} else {
			m["requestUrl"] = ""
		}
		m["userAgent"] = req.Request.UserAgent() // Empty string if not set
		m["referer"] = req.Request.Referer()     // Empty string if not set
		m["protocol"] = req.Request.Proto        // Empty string if not set
	}
	// GCP expects sizes as strings
	m["requestSize"] = strconv.FormatInt(req.RequestSize, 10)   // "0" if zero
	m["status"] = req.Status                                    // 0 if not set (though usually set)
	m["responseSize"] = strconv.FormatInt(req.ResponseSize, 10) // "0" if zero
	// GCP expects latency as "Ns" string (e.g., "3.5s")
	if req.Latency > 0 { // Only include latency if positive
		m["latency"] = fmt.Sprintf("%.9fs", req.Latency.Seconds())
	}
	m["remoteIp"] = req.RemoteIP // Empty string if not set
	m["serverIp"] = req.LocalIP  // Empty string if not set
	// Booleans are included directly
	m["cacheHit"] = req.CacheHit
	m["cacheValidatedWithOriginServer"] = req.CacheValidatedWithOriginServer
	m["cacheFillBytes"] = strconv.FormatInt(req.CacheFillBytes, 10) // "0" if zero
	m["cacheLookup"] = req.CacheLookup

	// Return the map, even if it only contains default values.
	return m
}

// convertToString converts any supported slog value type to a string suitable
// for use as a Cloud Logging label value. This follows GCP's requirements that
// all label values must be strings.
func convertToString(v any) string {
	if v == nil {
		return ""
	}

	switch val := v.(type) {
	case string:
		return val
	case int64:
		return strconv.FormatInt(val, 10)
	case uint64:
		return strconv.FormatUint(val, 10)
	case float64:
		return strconv.FormatFloat(val, 'g', -1, 64)
	case bool:
		if val {
			return "true"
		}
		return "false"
	case time.Time:
		return val.Format(time.RFC3339)
	case time.Duration:
		return val.String()
	default:
		// For any other type, use fmt.Sprintf
		return fmt.Sprintf("%v", val)
	}
}

// ExtractOperationFromRecord scans a slog.Record for a group named
// "logging.googleapis.com/operation" and builds a LogEntryOperation.
//
// The group may contain the keys "id", "producer", "first", and "last".
// Missing keys are treated as zero values. If no such group exists, nil is returned.
func ExtractOperationFromRecord(rec slog.Record) *logpb.LogEntryOperation {
	var op *logpb.LogEntryOperation

	rec.Attrs(func(a slog.Attr) bool {
		if a.Key != operationGroupKey || a.Value.Kind() != slog.KindGroup {
			return true
		}
		fields := a.Value.Group()
		o := &logpb.LogEntryOperation{}
		for i := range fields {
			switch fields[i].Key {
			case "id":
				o.Id = fields[i].Value.String()
			case "producer":
				o.Producer = fields[i].Value.String()
			case "first":
				o.First = fields[i].Value.Bool()
			case "last":
				o.Last = fields[i].Value.Bool()
			}
		}
		op = o
		// Stop walking; operation group found.
		return false
	})

	return op
}

// ExtractOperationFromPayload looks for a canonical
// "logging.googleapis.com/operation" object already present in a structured
// payload map and converts it into a LogEntryOperation. If the key is absent
// or the value is not a compatible map shape, it returns nil.
//
// This helper allows the handler to honor operation groups that were resolved
// into the payload (e.g., via initial attrs) before building the final entry.
func ExtractOperationFromPayload(payload map[string]any) *logpb.LogEntryOperation {
	if payload == nil {
		return nil
	}
	raw, ok := payload[operationGroupKey]
	if !ok || raw == nil {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}

	op := &logpb.LogEntryOperation{}

	if v, ok := m["id"]; ok {
		if s, ok := v.(string); ok {
			op.Id = s
		}
	}
	if v, ok := m["producer"]; ok {
		if s, ok := v.(string); ok {
			op.Producer = s
		}
	}
	if v, ok := m["first"]; ok {
		switch b := v.(type) {
		case bool:
			op.First = b
		case string:
			// tolerate stringified bools from user attrs
			op.First = b == "true"
		}
	}
	if v, ok := m["last"]; ok {
		switch b := v.(type) {
		case bool:
			op.Last = b
		case string:
			op.Last = b == "true"
		}
	}

	// If all fields are zero-values, treat as not present.
	if op.Id == "" && op.Producer == "" && !op.First && !op.Last {
		return nil
	}
	return op
}
