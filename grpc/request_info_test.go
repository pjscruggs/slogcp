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
