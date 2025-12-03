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
	"net"
	"net/http"
	"time"
)

// HTTPRequest mirrors the Cloud Logging HTTP request payload stored in
// LogEntry.http_request / httpRequest. It is shaped after the
// google.logging.type.HttpRequest proto message (see [Cloud Logging RPC])
// and matches the JSON schema described at [Cloud Logging REST].
// The corresponding Go proto type is
// [google.golang.org/genproto/googleapis/logging/type.HttpRequest].
//
// [Cloud Logging RPC]: https://cloud.google.com/logging/docs/reference/v2/rpc/google.logging.type#httprequest
// [Cloud Logging REST]: https://cloud.google.com/logging/docs/reference/v2/rest/v2/LogEntry#HttpRequest
type HTTPRequest struct {
	// Request is the original HTTP request. It is used for extracting
	// high-level details such as method, URL, headers, and protocol. The
	// request body is not logged.
	Request *http.Request

	// RequestMethod is the HTTP method, if known. When Request is non-nil this
	// is populated automatically.
	RequestMethod string

	// RequestURL is the full request URL string. When Request is non-nil this
	// is derived from Request.URL.
	RequestURL string

	// RequestSize is the size of the request in bytes. When unknown, it may be
	// set to -1.
	RequestSize int64

	// Status is the HTTP response status code.
	Status int

	// ResponseSize is the size of the response in bytes. When unknown, it may
	// be set to -1.
	ResponseSize int64

	// Latency is the time between when the request was received and when the
	// response was sent.
	Latency time.Duration

	// RemoteIP is the IP address of the client that sent the request.
	RemoteIP string

	// LocalIP is the IP address of the server that handled the request.
	LocalIP string

	// UserAgent is the user agent string of the client, if known.
	UserAgent string

	// Referer is the HTTP referer header of the request, if any.
	Referer string

	// Protocol is the protocol used for the request, such as "HTTP/1.1".
	Protocol string

	// CacheHit reports whether the response was served from cache.
	CacheHit bool

	// CacheValidatedWithOriginServer reports whether the cache entry was
	// validated with the origin server before being served.
	CacheValidatedWithOriginServer bool

	// CacheFillBytes is the number of bytes inserted into cache as a result of
	// the request.
	CacheFillBytes int64

	// CacheLookup reports whether the request was a cache lookup.
	CacheLookup bool
}

// PrepareHTTPRequest normalizes an HTTPRequest for logging by ensuring
// derived fields are populated and by detaching the original *http.Request to
// avoid repeated expansion during JSON serialization. The function is safe to
// call multiple times.
func PrepareHTTPRequest(req *HTTPRequest) {
	if req == nil {
		return
	}

	populateFromHTTPRequest(req)
	normalizeRemoteIP(req)
	normalizePayloadSizes(req)
}

// populateFromHTTPRequest copies fields from the embedded http.Request when present.
func populateFromHTTPRequest(req *HTTPRequest) {
	if req.Request == nil {
		return
	}
	r := req.Request
	populateRequestStrings(req, r)
	populateRequestSizes(req, r)
	populateRemoteIP(req, r.RemoteAddr)
	req.Request = nil
}

// populateRequestStrings fills request metadata fields when unset.
func populateRequestStrings(req *HTTPRequest, r *http.Request) {
	copyStringIfEmpty(&req.RequestMethod, r.Method)
	if r.URL != nil {
		copyStringIfEmpty(&req.RequestURL, r.URL.String())
	}
	copyStringIfEmpty(&req.UserAgent, r.UserAgent())
	copyStringIfEmpty(&req.Referer, r.Referer())
	copyStringIfEmpty(&req.Protocol, r.Proto)
}

// populateRequestSizes copies size information when available.
func populateRequestSizes(req *HTTPRequest, r *http.Request) {
	if req.RequestSize == 0 && r.ContentLength > 0 {
		req.RequestSize = r.ContentLength
	}
}

// populateRemoteIP sets the remote IP from the request when missing.
func populateRemoteIP(req *HTTPRequest, remoteAddr string) {
	copyStringIfEmpty(&req.RemoteIP, remoteAddr)
}

// copyStringIfEmpty assigns value to target when target is blank.
func copyStringIfEmpty(target *string, value string) {
	if target == nil || *target != "" || value == "" {
		return
	}
	*target = value
}

// normalizeRemoteIP strips ports from RemoteIP when possible.
func normalizeRemoteIP(req *HTTPRequest) {
	if req.RemoteIP == "" {
		return
	}
	if host, _, err := net.SplitHostPort(req.RemoteIP); err == nil {
		req.RemoteIP = host
	}
}

// normalizePayloadSizes standardizes negative size values to -1.
func normalizePayloadSizes(req *HTTPRequest) {
	if req.RequestSize < 0 {
		req.RequestSize = -1
	}
	if req.ResponseSize < 0 {
		req.ResponseSize = -1
	}
}

// HTTPRequestFromRequest constructs an HTTPRequest from a standard library
// *http.Request, normalizing it via PrepareHTTPRequest. It is useful when you
// need to emit Cloud Logging-compatible httpRequest payloads without using the
// slogcphttp middleware helpers.
func HTTPRequestFromRequest(r *http.Request) *HTTPRequest {
	if r == nil {
		return nil
	}
	req := &HTTPRequest{Request: r}
	PrepareHTTPRequest(req)
	return req
}

// LogValue implements slog.LogValuer so HTTPRequest payloads render using the
// Cloud Logging httpRequest schema even when emitted through generic slog
// handlers.
func (req *HTTPRequest) LogValue() slog.Value {
	if req == nil {
		return slog.Value{}
	}
	clone := *req
	return httpRequestPayloadValue(flattenHTTPRequestToMap(&clone))
}

// httpRequestPayloadValue converts an httpRequestPayload into a slog.Value while gracefully
// handling nil payloads.
func httpRequestPayloadValue(payload *httpRequestPayload) slog.Value {
	if payload == nil {
		return slog.Value{}
	}
	m := map[string]any{
		"requestMethod":                  payload.RequestMethod,
		"requestUrl":                     payload.RequestURL,
		"userAgent":                      payload.UserAgent,
		"referer":                        payload.Referer,
		"protocol":                       payload.Protocol,
		"requestSize":                    payload.RequestSize,
		"status":                         payload.Status,
		"responseSize":                   payload.ResponseSize,
		"remoteIp":                       payload.RemoteIP,
		"serverIp":                       payload.ServerIP,
		"cacheHit":                       payload.CacheHit,
		"cacheValidatedWithOriginServer": payload.CacheValidatedWithOriginServer,
		"cacheFillBytes":                 payload.CacheFillBytes,
		"cacheLookup":                    payload.CacheLookup,
	}
	if payload.Latency != "" {
		m["latency"] = payload.Latency
	}
	return slog.AnyValue(m)
}

// httpRequestFromValue extracts an *HTTPRequest pointer from a slog.Value if
// it was attached via slog.Any with a value that implements slog.LogValuer.
func httpRequestFromValue(v slog.Value) (*HTTPRequest, bool) {
	if v.Kind() == slog.KindLogValuer {
		return httpRequestFromLogValuer(v.LogValuer())
	}
	return nil, false
}

// httpRequestFromLogValuer unwraps HTTPRequest pointers stored as slog.LogValuer values.
func httpRequestFromLogValuer(v slog.LogValuer) (*HTTPRequest, bool) {
	switch hv := v.(type) {
	case *HTTPRequest:
		return hv, true
	case httpRequestLogValuer:
		if hv.build == nil {
			return nil, false
		}
		return hv.build(), true
	default:
		return nil, false
	}
}

// httpRequestLogValuer adapts a builder so httpRequest values can be resolved lazily.
type httpRequestLogValuer struct {
	build func() *HTTPRequest
}

// LogValue implements slog.LogValuer.
func (v httpRequestLogValuer) LogValue() slog.Value {
	if v.build == nil {
		return slog.Value{}
	}
	req := v.build()
	if req == nil {
		return slog.Value{}
	}
	return req.LogValue()
}

// HTTPRequestValue constructs a slog.Value that lazily builds an HTTPRequest.
func HTTPRequestValue(builder func() *HTTPRequest) slog.Value {
	return slog.AnyValue(httpRequestLogValuer{build: builder})
}
