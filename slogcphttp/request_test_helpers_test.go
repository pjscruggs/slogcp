// Copyright 2025-2026 Patrick J. Scruggs
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
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestRequest builds an httptest request tied to the test's context.
func newTestRequest(tb testing.TB, method, target string, body io.Reader) *http.Request {
	tb.Helper()

	return httptest.NewRequestWithContext(tb.Context(), method, target, body)
}
