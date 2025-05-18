//go:build smoke
// +build smoke

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

// Package http_test provides fast, hermetic smoke tests for the public
// HTTP middleware exported by the slogcp library.  Each test spins up an
// in‑memory httptest.Server, wraps a no‑op handler with the middleware,
// issues a single request, and asserts the response is 200 OK with no
// panics or unexpected errors.  These checks verify that the middleware
// can be imported, constructed, and executed by downstream users
// without touching the network or any external services.
package http_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pjscruggs/slogcp"
	slogcphttp "github.com/pjscruggs/slogcp/http"
)

func newTestLogger(t *testing.T) *slogcp.Logger {
	t.Helper()

	lg, err := slogcp.New(slogcp.WithRedirectToStdout())
	if err != nil {
		t.Fatalf("slogcp.New() returned %v, want nil", err)
	}
	return lg
}

// TestHTTPMiddlewareSmoke starts an in‑memory server with the middleware and
// checks that a GET / request completes with 200 OK.
func TestHTTPMiddlewareSmoke(t *testing.T) {
	t.Parallel()

	lg := newTestLogger(t)
	defer func() {
		if err := lg.Close(); err != nil {
			t.Errorf("Logger.Close() returned %v, want nil", err)
		}
	}()

	up := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	srv := httptest.NewServer(slogcphttp.Middleware(lg.Logger)(up))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("http.Get(%q) returned %v, want nil", srv.URL, err)
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		t.Errorf("GET %s status = %d, want %d", srv.URL, got, want)
	}
}
