package healthcheck

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMatchHTTPByPathAndTag(t *testing.T) {
	cfg := Config{
		Enabled:  true,
		Mode:     ModeTag,
		TagKey:   "hc",
		TagValue: true,
		Paths:    []string{"/healthz"},
	}
	f := NewFilter(cfg)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/healthz", nil)

	decision := f.MatchHTTP(req)
	if !decision.Matched {
		t.Fatal("expected request to match health-check filter")
	}
	if !decision.ShouldTag() || decision.TagKey != "hc" || !decision.TagValue {
		t.Fatalf("unexpected tag decision: %+v", decision)
	}
}

func TestMatchHTTPByForwardedIP(t *testing.T) {
	_, cidr, err := net.ParseCIDR("203.0.113.0/24")
	if err != nil {
		t.Fatalf("ParseCIDR failed: %v", err)
	}
	cfg := Config{
		Enabled:     true,
		Mode:        ModeDrop,
		RemoteCIDRs: []*net.IPNet{cidr},
	}
	f := NewFilter(cfg)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.42, 198.51.100.1")

	decision := f.MatchHTTP(req)
	if !decision.Matched {
		t.Fatal("expected forwarded IP to be recognised as health check")
	}
	if decision.Mode != ModeDrop {
		t.Fatalf("decision mode = %q, want %q", decision.Mode, ModeDrop)
	}
	if decision.ShouldTag() {
		t.Fatal("drop mode should not tag records")
	}
}

func TestMatchGRPCByMethodAndApplyLevel(t *testing.T) {
	demote := slog.LevelDebug
	cfg := Config{
		Enabled:    true,
		Mode:       ModeDemote,
		TagKey:     "is_health_check",
		TagValue:   true,
		DemoteTo:   &demote,
		Methods:    []string{"/grpc.health.v1.Health/Check"},
		HeaderEquals: map[string][]string{
			"user-agent": {"GoogleHC/1.0"},
		},
	}
	f := NewFilter(cfg)
	metadata := map[string][]string{"user-agent": {"GoogleHC/1.0"}}
	decision := f.MatchGRPC("/grpc.health.v1.Health/Check", metadata, "")
	if !decision.Matched {
		t.Fatal("expected gRPC health method to match")
	}
	if !decision.HasDemote {
		t.Fatal("expected demote flag to be set")
	}
	if got := decision.ApplyLevel(slog.LevelInfo); got != slog.LevelDebug {
		t.Fatalf("ApplyLevel returned %v, want %v", got, slog.LevelDebug)
	}
	if !decision.ShouldTag() {
		t.Fatal("expected tag to be emitted in demote mode")
	}
}

func TestContextRoundTrip(t *testing.T) {
	decision := Decision{
		Matched:     true,
		Mode:        ModeDemote,
		TagKey:      "hc",
		TagValue:    true,
		DemoteLevel: slog.LevelDebug,
		HasDemote:   true,
	}
	ctx := ContextWithDecision(context.Background(), decision)
	got, ok := DecisionFromContext(ctx)
	if !ok {
		t.Fatal("expected decision to be retrievable from context")
	}
	if got.Mode != decision.Mode || got.TagKey != decision.TagKey || !got.TagValue {
		t.Fatalf("decision mismatch: got %+v want %+v", got, decision)
	}
}
