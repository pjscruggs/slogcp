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

package http

import (
	"log/slog"
	stdhttp "net/http"

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
func HTTPRequestAttr(r *stdhttp.Request, scope *RequestScope) slog.Attr {
	if r == nil && scope == nil {
		return slog.Attr{}
	}

	req := &slogcp.HTTPRequest{
		Request: r,
	}
	if scope != nil {
		req.RequestMethod = scope.Method()
		req.RequestSize = scope.RequestSize()
		req.ResponseSize = scope.ResponseSize()
		req.Status = scope.Status()
		req.Latency = scope.Latency()
		req.RemoteIP = scope.ClientIP()
		req.UserAgent = scope.UserAgent()
	}
	slogcp.PrepareHTTPRequest(req)
	return slog.Any(httpRequestKey, req)
}
