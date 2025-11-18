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

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	slogcphttp "github.com/pjscruggs/slogcp/http"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestTransportForwardsTraceHeaders validates that the example transport
// injects W3C trace headers when an active span is present.
func TestTransportForwardsTraceHeaders(t *testing.T) {
	t.Parallel()

	prevTP := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tp.Shutdown(ctx)
		otel.SetTracerProvider(prevTP)
	})

	traceCh := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Traceparent")
		traceCh <- header
		if header != "" {
			w.Header().Set("X-Received-Trace", "true")
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	client := &http.Client{Transport: slogcphttp.Transport(nil)}

	ctx, span := otel.Tracer("example/http-client").Start(context.Background(), "outbound")
	defer span.End()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var traceHeader string
	select {
	case traceHeader = <-traceCh:
	case <-time.After(time.Second):
		t.Fatalf("request header capture timed out")
	}
	if traceHeader == "" {
		t.Fatalf("expected traceparent header to be set")
	}

	if got := resp.Header.Get("X-Received-Trace"); got != "true" {
		t.Fatalf("X-Received-Trace = %q, want %q", got, "true")
	}
}
