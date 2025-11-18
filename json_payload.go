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
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Constants for standard keys used within the structured log payload.
const (
	messageKey     = "message"
	stackTraceKey  = "stack_trace"
	httpRequestKey = "httpRequest"
	labelsGroupKey = "logging.googleapis.com/labels"
)

// formattedError holds the processed error details for structured logging.
type formattedError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// formatErrorForReporting prepares an error by extracting its type and message
// and optionally capturing an origin stack trace.
func formatErrorForReporting(err error) (fe formattedError, originStackTrace string) {
	if err == nil {
		return formattedError{Message: "<nil error>"}, ""
	}

	fe = formattedError{
		Message: err.Error(),
		Type:    fmt.Sprintf("%T", err),
	}

	originStackTrace = extractAndFormatOriginStack(err)
	return fe, originStackTrace
}

// resolveSlogValue converts an slog.Value into a Go type suitable for JSON
// marshalling within the payload map.
func resolveSlogValue(v slog.Value) any {
	rv := v.Resolve()

	switch rv.Kind() {
	case slog.KindGroup:
		groupAttrs := rv.Group()
		if len(groupAttrs) == 0 {
			return nil
		}
		groupMap := make(map[string]any, len(groupAttrs))
		for _, ga := range groupAttrs {
			if ga.Key == "" {
				continue
			}
			resolvedGroupVal := resolveSlogValue(ga.Value)
			if resolvedGroupVal != nil {
				groupMap[ga.Key] = resolvedGroupVal
			}
		}
		if len(groupMap) == 0 {
			return nil
		}
		return groupMap
	case slog.KindBool:
		return rv.Bool()
	case slog.KindDuration:
		return rv.Duration().String()
	case slog.KindFloat64:
		return rv.Float64()
	case slog.KindInt64:
		return rv.Int64()
	case slog.KindString:
		return rv.String()
	case slog.KindTime:
		return rv.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindUint64:
		return rv.Uint64()
	case slog.KindAny:
		fallthrough
	default:
		val := rv.Any()
		switch vt := val.(type) {
		case error:
			return vt.Error()
		case *HTTPRequest:
			return vt
		case *http.Request:
			return nil
		}
		if val == nil {
			return nil
		}
		return val
	}
}

type httpRequestPayload struct {
	RequestMethod                  string `json:"requestMethod"`
	RequestURL                     string `json:"requestUrl"`
	UserAgent                      string `json:"userAgent"`
	Referer                        string `json:"referer"`
	Protocol                       string `json:"protocol"`
	RequestSize                    string `json:"requestSize"`
	Status                         int    `json:"status"`
	ResponseSize                   string `json:"responseSize"`
	Latency                        string `json:"latency,omitempty"`
	RemoteIP                       string `json:"remoteIp"`
	ServerIP                       string `json:"serverIp"`
	CacheHit                       bool   `json:"cacheHit"`
	CacheValidatedWithOriginServer bool   `json:"cacheValidatedWithOriginServer"`
	CacheFillBytes                 string `json:"cacheFillBytes"`
	CacheLookup                    bool   `json:"cacheLookup"`
}

// flattenHTTPRequestToMap converts an HTTPRequest into a JSON-friendly payload for httpRequest fields.
func flattenHTTPRequestToMap(req *HTTPRequest) *httpRequestPayload {
	if req == nil {
		return nil
	}

	if req.Request != nil {
		PrepareHTTPRequest(req)
	}

	payload := httpRequestPayload{
		RequestMethod:                  req.RequestMethod,
		RequestURL:                     req.RequestURL,
		UserAgent:                      req.UserAgent,
		Referer:                        req.Referer,
		Protocol:                       req.Protocol,
		RequestSize:                    strconv.FormatInt(req.RequestSize, 10),
		Status:                         req.Status,
		ResponseSize:                   strconv.FormatInt(req.ResponseSize, 10),
		RemoteIP:                       req.RemoteIP,
		ServerIP:                       req.LocalIP,
		CacheHit:                       req.CacheHit,
		CacheValidatedWithOriginServer: req.CacheValidatedWithOriginServer,
		CacheFillBytes:                 strconv.FormatInt(req.CacheFillBytes, 10),
		CacheLookup:                    req.CacheLookup,
	}

	if req.Latency > 0 {
		payload.Latency = fmt.Sprintf("%.9fs", req.Latency.Seconds())
	}

	return &payload
}

// labelValueToString converts a slog.Value into its string form suitable for label emission.
func labelValueToString(v slog.Value) (string, bool) {
	rv := v.Resolve()
	switch rv.Kind() {
	case slog.KindString:
		return rv.String(), true
	case slog.KindInt64:
		return strconv.FormatInt(rv.Int64(), 10), true
	case slog.KindUint64:
		return strconv.FormatUint(rv.Uint64(), 10), true
	case slog.KindFloat64:
		return strconv.FormatFloat(rv.Float64(), 'g', -1, 64), true
	case slog.KindBool:
		if rv.Bool() {
			return "true", true
		}
		return "false", true
	case slog.KindDuration:
		return rv.Duration().String(), true
	case slog.KindTime:
		return rv.Time().Format(time.RFC3339), true
	case slog.KindAny:
		val := rv.Any()
		if s, ok := val.(fmt.Stringer); ok {
			return s.String(), true
		}
		if val == nil {
			return "", false
		}
		return fmt.Sprintf("%v", val), true
	default:
		return "", false
	}
}

// cloneStringMap clones a string map while preserving nil for empty inputs.
func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dup := make(map[string]string, len(src))
	for k, v := range src {
		dup[k] = v
	}
	return dup
}

// stringMapToAny converts a string map into a map[string]any while preserving keys.
func stringMapToAny(src map[string]string) map[string]any {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
