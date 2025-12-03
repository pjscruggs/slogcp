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

package slogcphttp

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/pjscruggs/slogcp"
)

const httpRequestKey = "httpRequest"

// HTTPRequestAttr builds a slog attribute compatible with Cloud Logging's
// [httpRequest field]. Callers can pass the original *http.Request and an
// optional RequestScope captured from the context. Only basic metadata is
// populated; the attribute is opt-in and not attached by middleware
// automatically.
//
// [httpRequest field]: https://docs.cloud.google.com/logging/docs/reference/v2/rest/v2/LogEntry#HttpRequest
func HTTPRequestAttr(r *http.Request, scope *RequestScope) slog.Attr {
	if r == nil && scope == nil {
		return slog.Attr{}
	}

	builder := func() *slogcp.HTTPRequest {
		req := &slogcp.HTTPRequest{Request: r}
		if scope != nil {
			req.RequestMethod = scope.Method()
			req.RequestSize = scope.RequestSize()
			req.ResponseSize = scope.ResponseSize()
			req.Status = scope.Status()
			req.Latency = latencyForLogging(scope)
			req.RemoteIP = scope.ClientIP()
			req.UserAgent = scope.UserAgent()
			if req.RequestURL == "" {
				req.RequestURL = buildRequestURLFromScope(scope)
			}
		}
		return req
	}

	return slog.Attr{Key: httpRequestKey, Value: slogcp.HTTPRequestValue(builder)}
}

// HTTPRequestFromScope snapshots the provided RequestScope into a Cloud Logging httpRequest payload.
// It is useful for one-shot access logs emitted at the end of a request.
func HTTPRequestFromScope(scope *RequestScope) *slogcp.HTTPRequest {
	if scope == nil {
		return nil
	}
	req := &slogcp.HTTPRequest{
		RequestMethod: scope.Method(),
		RequestURL:    buildRequestURLFromScope(scope),
		RequestSize:   scope.RequestSize(),
		ResponseSize:  scope.ResponseSize(),
		Status:        scope.Status(),
		Latency:       latencyForLogging(scope),
		RemoteIP:      scope.ClientIP(),
		UserAgent:     scope.UserAgent(),
	}
	slogcp.PrepareHTTPRequest(req)
	return req
}

// latencyForLogging returns finalized latency when available; otherwise returns a sentinel (-1s)
// so Cloud Logging promotes httpRequest but omits latency on mid-request logs.
func latencyForLogging(scope *RequestScope) time.Duration {
	if scope == nil {
		return 0
	}
	if d, ok := scope.Latency(); ok {
		return d
	}
	return -time.Second
}

// buildRequestURLFromScope reconstructs a URL string from scope fields when available.
func buildRequestURLFromScope(scope *RequestScope) string {
	if scope == nil {
		return ""
	}
	scheme := strings.TrimSpace(scope.Scheme())
	host := strings.TrimSpace(scope.Host())
	target := scope.Target()
	query := scope.Query()

	var b strings.Builder
	if scheme != "" && host != "" {
		b.WriteString(scheme)
		b.WriteString("://")
		b.WriteString(host)
	}
	if target != "" {
		b.WriteString(target)
	}
	if query != "" {
		b.WriteByte('?')
		b.WriteString(query)
	}
	return b.String()
}

// HTTPRequestAttrFromContext returns the Cloud Logging httpRequest attribute by
// retrieving the RequestScope stored in ctx (if any). Callers can pass the
// original *http.Request so derived metadata such as headers are preserved.
func HTTPRequestAttrFromContext(ctx context.Context, r *http.Request) slog.Attr {
	scope, _ := ScopeFromContext(ctx)
	return HTTPRequestAttr(r, scope)
}

// HTTPRequestEnricher is an AttrEnricher that appends the Cloud Logging
// httpRequest payload when data is available. It can be supplied to
// WithAttrEnricher or used directly for custom instrumentation.
func HTTPRequestEnricher(r *http.Request, scope *RequestScope) []slog.Attr {
	attr := HTTPRequestAttr(r, scope)
	if attr.Key == "" {
		return nil
	}
	return []slog.Attr{attr}
}

var _ AttrEnricher = HTTPRequestEnricher
