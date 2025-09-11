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

//go:build unit
// +build unit

package http

import (
	"context"
	"io"
	stdhttp "net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func mustTraceID(hexStr string) trace.TraceID {
	id, err := trace.TraceIDFromHex(hexStr)
	if err != nil {
		panic(err)
	}
	return id
}

func mustSpanID(hexStr string) trace.SpanID {
	id, err := trace.SpanIDFromHex(hexStr)
	if err != nil {
		panic(err)
	}
	return id
}

func TestInjectTraceContextFromHeader(t *testing.T) {
	traceHex := "70f5c2c7b3c0d8eead4837399ac5b327"

	ctx := injectTraceContextFromHeader(context.Background(), traceHex)
	sc := trace.SpanContextFromContext(ctx)
	if sc.TraceID().String() != traceHex || !sc.SpanID().IsValid() || sc.IsSampled() {
		t.Errorf("injectTraceContextFromHeader(no span) = %v", sc)
	}

	header := traceHex + "/6891007561858694673"
	ctx2 := injectTraceContextFromHeader(context.Background(), header)
	sc2 := trace.SpanContextFromContext(ctx2)
	if sc2.TraceID().String() != traceHex || sc2.SpanID().String() != "5fa1c6de0d1e3e11" || sc2.IsSampled() {
		t.Errorf("injectTraceContextFromHeader(with span) = %v", sc2)
	}
}

type captureRoundTripper struct{ hdr stdhttp.Header }

func (c *captureRoundTripper) RoundTrip(req *stdhttp.Request) (*stdhttp.Response, error) {
	c.hdr = req.Header.Clone()
	return &stdhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

func newRequestWithSpan() *stdhttp.Request {
	req, _ := stdhttp.NewRequest("GET", "http://example.com", nil)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    mustTraceID("70f5c2c7b3c0d8eead4837399ac5b327"),
		SpanID:     mustSpanID("5fa1c6de0d1e3e11"),
		TraceFlags: trace.FlagsSampled,
	})
	req = req.WithContext(trace.ContextWithSpanContext(context.Background(), sc))
	return req
}

func TestTracePropagationTransport(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	cap := &captureRoundTripper{}
	tp := TracePropagationTransport{Base: cap}
	if _, err := tp.RoundTrip(newRequestWithSpan()); err != nil {
		t.Fatalf("RoundTrip returned %v", err)
	}
	if cap.hdr.Get("traceparent") == "" || cap.hdr.Get(XCloudTraceContextHeader) == "" {
		t.Errorf("headers not injected: %v", cap.hdr)
	}

	cap2 := &captureRoundTripper{}
	tp2 := TracePropagationTransport{Base: cap2, Skip: func(*stdhttp.Request) bool { return true }}
	if _, err := tp2.RoundTrip(newRequestWithSpan()); err != nil {
		t.Fatalf("RoundTrip returned %v", err)
	}
	if cap2.hdr.Get("traceparent") != "" || cap2.hdr.Get(XCloudTraceContextHeader) != "" {
		t.Errorf("headers injected despite skip: %v", cap2.hdr)
	}
}

func TestNewTraceRoundTripper(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	cap := &captureRoundTripper{}
	rt := NewTraceRoundTripper(cap)
	if _, err := rt.RoundTrip(newRequestWithSpan()); err != nil {
		t.Fatalf("RoundTrip returned %v", err)
	}
	if cap.hdr.Get("traceparent") == "" || cap.hdr.Get(XCloudTraceContextHeader) == "" {
		t.Errorf("headers not injected: %v", cap.hdr)
	}

	cap2 := &captureRoundTripper{}
	rt2 := NewTraceRoundTripper(cap2, WithSkip(func(*stdhttp.Request) bool { return true }))
	if _, err := rt2.RoundTrip(newRequestWithSpan()); err != nil {
		t.Fatalf("RoundTrip returned %v", err)
	}
	if cap2.hdr.Get("traceparent") != "" || cap2.hdr.Get(XCloudTraceContextHeader) != "" {
		t.Errorf("headers injected despite skip: %v", cap2.hdr)
	}
}
