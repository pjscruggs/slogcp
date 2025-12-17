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
	"net/http"
	"strings"

	"log/slog"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace/noop"
)

// alwaysIncludeFilter accepts all requests and is used to exercise filter hooks.
var alwaysIncludeFilter = func(*http.Request) bool { return true }

// TestWithLoggerHandlesNil verifies WithLogger falls back to slog.Default().
func TestWithLoggerHandlesNil(t *testing.T) {
	t.Parallel()

	cfg := applyOptions([]Option{WithLogger(nil)})
	if cfg.logger != slog.Default() {
		t.Fatalf("WithLogger(nil) did not restore slog.Default()")
	}

	custom := slog.New(slog.DiscardHandler)
	cfg = applyOptions([]Option{WithLogger(custom)})
	if cfg.logger != custom {
		t.Fatalf("WithLogger custom logger mismatch")
	}
}

// TestWithHTTPRequestAttr toggles automatic httpRequest enrichment flag.
func TestWithHTTPRequestAttr(t *testing.T) {
	t.Parallel()

	cfg := applyOptions([]Option{WithHTTPRequestAttr(true)})
	if !cfg.includeHTTPRequestAttr {
		t.Fatalf("includeHTTPRequestAttr = false, want true")
	}

	cfg = applyOptions([]Option{WithHTTPRequestAttr(false)})
	if cfg.includeHTTPRequestAttr {
		t.Fatalf("includeHTTPRequestAttr = true, want false")
	}
}

// TestOptionSettersCoverAllFields validates individual option helpers update config fields.
func TestOptionSettersCoverAllFields(t *testing.T) {
	t.Parallel()

	prop := propagation.TraceContext{}
	tp := noop.NewTracerProvider()
	var filterHit bool
	filter := func(*http.Request) bool {
		filterHit = true
		return true
	}
	formatter := func(name string, _ *http.Request) string {
		return name + "-fmt"
	}

	cfg := applyOptions([]Option{
		WithPropagators(prop),
		WithTracerProvider(tp),
		WithTracePropagation(false),
		WithPublicEndpoint(true),
		WithPublicEndpointCorrelateLogsToRemote(true),
		WithOTel(false),
		WithSpanNameFormatter(formatter),
		WithFilter(nil), // ensure nil is skipped
		WithFilter(filter),
		WithClientIP(false),
		WithProxyMode(ProxyModeGCLB),
		WithXForwardedForClientIPFromRight(3),
	})

	if !cfg.propagatorsSet || cfg.propagators != prop {
		t.Fatalf("propagators not applied: %#v", cfg.propagators)
	}
	if cfg.tracerProvider != tp {
		t.Fatalf("tracerProvider not applied")
	}
	if cfg.propagateTrace {
		t.Fatalf("propagateTrace should be false after WithTracePropagation(false)")
	}
	if !cfg.publicEndpoint {
		t.Fatalf("publicEndpoint should be true")
	}
	if !cfg.publicEndpointCorrelateLogsToRemote {
		t.Fatalf("publicEndpointCorrelateLogsToRemote should be true")
	}
	if cfg.enableOTel {
		t.Fatalf("enableOTel should be false after WithOTel(false)")
	}
	if cfg.includeClientIP {
		t.Fatalf("includeClientIP should be false after WithClientIP(false)")
	}
	if cfg.proxyMode != ProxyModeGCLB {
		t.Fatalf("proxyMode = %v, want ProxyModeGCLB", cfg.proxyMode)
	}
	if cfg.xffClientIPFromRight != 3 {
		t.Fatalf("xffClientIPFromRight = %d, want 3", cfg.xffClientIPFromRight)
	}
	if out := cfg.spanNameFormatter("base", nil); out != "base-fmt" {
		t.Fatalf("spanNameFormatter output = %q", out)
	}
	if len(cfg.filters) != 1 {
		t.Fatalf("filters len = %d, want 1", len(cfg.filters))
	}
	cfg.filters[0](nil)
	if !filterHit {
		t.Fatalf("filter was not invoked")
	}
}

// TestOtelOptionsCoversBranches ensures otelOptions emits entries for every non-empty field.
func TestOtelOptionsCoversBranches(t *testing.T) {
	t.Parallel()

	cfg := &config{
		tracerProvider:    noop.NewTracerProvider(),
		propagators:       propagation.TraceContext{},
		propagatorsSet:    true,
		publicEndpoint:    true,
		spanNameFormatter: func(name string, _ *http.Request) string { return strings.ToUpper(name) },
		filters: []otelhttp.Filter{
			nil,
			alwaysIncludeFilter,
		},
	}

	opts := otelOptions(cfg)
	if got := len(opts); got != 5 {
		t.Fatalf("otelOptions len = %d, want 5", got)
	}
}
