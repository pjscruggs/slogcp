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

package grpc

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
)

// TestRequestInfoKindAndClient ensures helpers expose the stored kind/client flags.
func TestRequestInfoKindAndClient(t *testing.T) {
	t.Parallel()

	start := time.Now()
	info := newRequestInfo("/pkg.Service/BidiStream", "client_stream", true, start)
	if info.Kind() != "client_stream" {
		t.Fatalf("Kind() = %q, want client_stream", info.Kind())
	}
	if !info.IsClient() {
		t.Fatalf("IsClient() = false, want true")
	}

	info.finalize(codes.Internal, -5*time.Millisecond)
	if info.Status() != codes.Internal {
		t.Fatalf("Status() = %v, want Internal", info.Status())
	}
	if latency := info.Latency(); latency < 0 {
		t.Fatalf("Latency() = %v, want non-negative", latency)
	}
}

// TestSplitFullMethod parses streaming method names and handles missing prefixes.
func TestSplitFullMethod(t *testing.T) {
	t.Parallel()

	service, method := splitFullMethod("/pkg.Service/Bidi")
	if service != "pkg.Service" || method != "Bidi" {
		t.Fatalf("splitFullMethod parsed %q/%q, want pkg.Service/Bidi", service, method)
	}

	service, method = splitFullMethod("UnqualifiedMethod")
	if service != "" || method != "UnqualifiedMethod" {
		t.Fatalf("splitFullMethod unqualified output = %q/%q", service, method)
	}
}

// TestMessageSizeCoversInterfaces validates the Size interface branch.
func TestMessageSizeCoversInterfaces(t *testing.T) {
	t.Parallel()

	msg := fakeSizer(42)
	if got := messageSize(msg); got != 42 {
		t.Fatalf("messageSize(fakeSizer) = %d, want 42", got)
	}
	if got := messageSize(&msg); got != 42 {
		t.Fatalf("messageSize(pointer fakeSizer) = %d, want 42", got)
	}
	if got := messageSize(struct{}{}); got != 0 {
		t.Fatalf("messageSize(struct) = %d, want 0", got)
	}
}

type fakeSizer int

// Size reports the fake encoded size for messageSize tests.
func (f fakeSizer) Size() int { return int(f) }

// TestRequestInfoRecordMetrics ensures request/response accounting handles nils and valid payloads.
func TestRequestInfoRecordMetrics(t *testing.T) {
	t.Parallel()

	info := newRequestInfo("/pkg.Service/Unary", "unary", false, time.Now())
	info.recordRequest(nil)
	info.recordResponse(nil)
	if info.RequestCount() != 0 || info.ResponseCount() != 0 {
		t.Fatalf("record* should ignore nil payloads, got req=%d resp=%d", info.RequestCount(), info.ResponseCount())
	}

	info.recordRequest(fakeSizer(11))
	info.recordResponse(fakeSizer(7))
	if info.RequestBytes() != 11 || info.ResponseBytes() != 7 {
		t.Fatalf("bytes tracked incorrectly, req=%d resp=%d", info.RequestBytes(), info.ResponseBytes())
	}
	if info.RequestCount() != 1 || info.ResponseCount() != 1 {
		t.Fatalf("counts tracked incorrectly, req=%d resp=%d", info.RequestCount(), info.ResponseCount())
	}
}

// TestRequestInfoLatencyFallback reports elapsed time before explicit finalization.
func TestRequestInfoLatencyFallback(t *testing.T) {
	t.Parallel()

	start := time.Now().Add(-10 * time.Millisecond)
	info := newRequestInfo("/pkg.Service/Unary", "unary", false, start)
	if got := info.Latency(); got <= 0 {
		t.Fatalf("Latency before finalize = %v, want >0", got)
	}

	info.finalize(codes.OK, 5*time.Millisecond)
	if got := info.Latency(); got != 5*time.Millisecond {
		t.Fatalf("Latency after finalize = %v, want 5ms", got)
	}
}

// TestSplitFullMethodHandlesServiceOnly ensures service-only identifiers parse correctly.
func TestSplitFullMethodHandlesServiceOnly(t *testing.T) {
	t.Parallel()

	service, method := splitFullMethod("/pkg.Service")
	if service != "pkg.Service" || method != "" {
		t.Fatalf("splitFullMethod service-only parsed %q/%q", service, method)
	}
}
