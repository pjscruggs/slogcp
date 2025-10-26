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
	"net"
	"net/http"
	"time"
)

// HTTPRequest represents the common subset of Google Cloud Logging HTTP request
// metadata captured by slogcp's HTTP middleware. It mirrors the public API
// surface of cloud.google.com/go/logging.HTTPRequest.
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

	if r := req.Request; r != nil {
		if req.RequestMethod == "" {
			req.RequestMethod = r.Method
		}
		if req.RequestURL == "" && r.URL != nil {
			req.RequestURL = r.URL.String()
		}
		if req.UserAgent == "" {
			req.UserAgent = r.UserAgent()
		}
		if req.Referer == "" {
			req.Referer = r.Referer()
		}
		if req.Protocol == "" {
			req.Protocol = r.Proto
		}
		if req.RequestSize == 0 && r.ContentLength > 0 {
			req.RequestSize = r.ContentLength
		}
		if req.RemoteIP == "" {
			req.RemoteIP = r.RemoteAddr
		}
		req.Request = nil
	}

	if req.RemoteIP != "" {
		if host, _, err := net.SplitHostPort(req.RemoteIP); err == nil {
			req.RemoteIP = host
		}
	}

	if req.RequestSize < 0 {
		req.RequestSize = -1
	}
	if req.ResponseSize < 0 {
		req.ResponseSize = -1
	}
}
