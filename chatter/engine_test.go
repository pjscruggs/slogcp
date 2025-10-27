package chatter

import (
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"
)

// TestSelectForwardedIP exercises edge cases when selecting client IPs from forwarded headers.
func TestSelectForwardedIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		values      []string
		trustedHops int
		want        string
	}{
		{
			name:        "zeroTrustedHopsReturnsLastEntry",
			values:      []string{"203.0.113.1, 198.51.100.2"},
			trustedHops: 0,
			want:        "198.51.100.2",
		},
		{
			name:        "trustedHopSelectsClientIP",
			values:      []string{"203.0.113.1, 198.51.100.2"},
			trustedHops: 1,
			want:        "203.0.113.1",
		},
		{
			name:        "skipsEmptyEntriesAcrossMultipleHeaders",
			values:      []string{" , , ", " 203.0.113.5 , "},
			trustedHops: 0,
			want:        "203.0.113.5",
		},
		{
			name:        "combinesMultipleHeaderValuesBeforeSelecting",
			values:      []string{"198.51.100.10", "198.51.100.20"},
			trustedHops: 1,
			want:        "198.51.100.10",
		},
		{
			name:        "outOfRangeTrustedHopReturnsEmpty",
			values:      []string{"198.51.100.2"},
			trustedHops: 5,
			want:        "",
		},
		{
			name:        "negativeTrustedHopTreatsAsZero",
			values:      []string{"198.51.100.10, 198.51.100.20"},
			trustedHops: -3,
			want:        "198.51.100.20",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if got := selectForwardedIP(tt.values, tt.trustedHops); got != tt.want {
				t.Fatalf("selectForwardedIP(%q, %d) = %q, want %q", tt.values, tt.trustedHops, got, tt.want)
			}
		})
	}
}

// TestEngineExtractClientIP validates Engine.extractClientIP selection and trust flags.
func TestEngineExtractClientIP(t *testing.T) {
	t.Parallel()

	mustCIDR := func(t *testing.T, cidr string) *net.IPNet {
		t.Helper()
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("ParseCIDR(%q): %v", cidr, err)
		}
		return network
	}

	tests := []struct {
		name      string
		engine    *Engine
		remote    string
		forwarded []string
		wantIP    string
		wantTrust bool
	}{
		{
			name: "noProxyTrustUsesRemoteAddr",
			engine: &Engine{
				proxy: proxyRuntime{trust: false},
			},
			remote:    "192.0.2.10:443",
			forwarded: []string{"198.51.100.1"},
			wantIP:    "192.0.2.10",
			wantTrust: true,
		},
		{
			name: "trustWithoutCIDRsFailsOpen",
			engine: &Engine{
				proxy: proxyRuntime{
					trust: true,
				},
			},
			remote:    "10.0.0.1:8080",
			forwarded: []string{"203.0.113.7"},
			wantIP:    "10.0.0.1",
			wantTrust: false,
		},
		{
			name: "trustedCIDRWithForwardedIP",
			engine: &Engine{
				proxy: proxyRuntime{
					trust:        true,
					trustedCIDRs: []*net.IPNet{mustCIDR(t, "10.0.0.0/8")},
					trustedHops:  0,
				},
			},
			remote:    "10.1.2.3:9000",
			forwarded: []string{"203.0.113.55"},
			wantIP:    "203.0.113.55",
			wantTrust: true,
		},
		{
			name: "invalidForwardedIPFallsBack",
			engine: &Engine{
				proxy: proxyRuntime{
					trust:        true,
					trustedCIDRs: []*net.IPNet{mustCIDR(t, "10.0.0.0/8")},
					trustedHops:  0,
				},
			},
			remote:    "10.1.2.3:443",
			forwarded: []string{"not-an-ip"},
			wantIP:    "10.1.2.3",
			wantTrust: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			gotIP, gotTrust := tt.engine.extractClientIP(tt.remote, tt.forwarded)
			if gotIP != tt.wantIP || gotTrust != tt.wantTrust {
				t.Fatalf("extractClientIP() = (%q, %v), want (%q, %v)", gotIP, gotTrust, tt.wantIP, tt.wantTrust)
			}
		})
	}
}

// TestMatchAppEngineCron ensures cron header detection produces the expected decisions.
func TestMatchAppEngineCron(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "http://example.com/tasks", nil)
	req.Header.Set("X-Appengine-Cron", " TrUe ")

	decision := matchAppEngineCron(req, ActionMark)
	if decision == nil {
		t.Fatalf("matchAppEngineCron() returned nil, want decision")
	}
	if decision.Action != ActionMark || decision.Reason != ReasonAppEngineCron || decision.Rule != "X-Appengine-Cron" {
		t.Fatalf("unexpected decision: %#v", decision)
	}

	if got := matchAppEngineCron(httptest.NewRequest(http.MethodGet, "/", nil), ActionDrop); got != nil {
		t.Fatalf("expected nil for missing cron header")
	}
}

// TestMatchAppEnginePath verifies App Engine path suppression behaves according to configuration.
func TestMatchAppEnginePath(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "http://example.com/_ah/health", nil)
	if decision := matchAppEnginePath(req, true); decision == nil || decision.Reason != ReasonAppEngineAH || decision.Rule != "/_ah/health" {
		t.Fatalf("matchAppEnginePath() = %#v, want App Engine suppression decision", decision)
	}

	if decision := matchAppEnginePath(req, false); decision != nil {
		t.Fatalf("expected nil when suppression disabled")
	}
}

// TestMatchHTTPUserRules covers path, prefix, regex, and header matching for user rules.
func TestMatchHTTPUserRules(t *testing.T) {
	t.Parallel()

	rt := httpRuntime{
		paths: map[string]struct{}{"/healthz": {}},
		prefixes: []string{
			"/admin",
		},
		regex: []*regexp.Regexp{
			regexp.MustCompile(`^/tasks/\d+$`),
		},
		headers: []headerMatcher{
			{
				key: "X-Env",
				values: map[string]struct{}{
					"prod": {},
				},
			},
		},
	}

	pathReq := httptest.NewRequest(http.MethodGet, "http://example.com/healthz", nil)
	if reason, rule, ok := matchHTTPUserRules(pathReq, rt); !ok || reason != ReasonPath || rule != "/healthz" {
		t.Fatalf("expected path match, got reason=%v rule=%q ok=%v", reason, rule, ok)
	}

	prefixReq := httptest.NewRequest(http.MethodGet, "http://example.com/admin/dashboard", nil)
	if reason, rule, ok := matchHTTPUserRules(prefixReq, rt); !ok || reason != ReasonPrefix || rule != "/admin" {
		t.Fatalf("expected prefix match, got reason=%v rule=%q ok=%v", reason, rule, ok)
	}

	regexReq := httptest.NewRequest(http.MethodGet, "http://example.com/tasks/42", nil)
	if reason, rule, ok := matchHTTPUserRules(regexReq, rt); !ok || reason != ReasonRegex || rule != `^/tasks/\d+$` {
		t.Fatalf("expected regex match, got reason=%v rule=%q ok=%v", reason, rule, ok)
	}

	headerReq := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	headerReq.Header.Set("X-Env", "PROD")
	if reason, rule, ok := matchHTTPUserRules(headerReq, rt); !ok || reason != ReasonHeader || rule != "X-Env=prod" {
		t.Fatalf("expected header match, got reason=%v rule=%q ok=%v", reason, rule, ok)
	}

	if reason, rule, ok := matchHTTPUserRules(httptest.NewRequest(http.MethodGet, "http://example.com/other", nil), rt); ok || reason != ReasonUnknown || rule != "" {
		t.Fatalf("expected miss, got reason=%v rule=%q ok=%v", reason, rule, ok)
	}
}

// TestHeaderMatcher checks case-insensitive matching for configured header values.
func TestHeaderMatcher(t *testing.T) {
	t.Parallel()

	hm := headerMatcher{
		key: "X-Test",
		values: map[string]struct{}{
			"match": {},
		},
	}
	header := http.Header{}
	header.Add("X-Test", "  MATCH ")

	if !hm.match(header) {
		t.Fatalf("expected matcher to accept header")
	}

	header.Del("X-Test")
	header.Add("X-Test", "miss")
	if hm.match(header) {
		t.Fatalf("expected matcher to reject header value")
	}
}

// TestMatchCIDR confirms CIDR matching accepts valid IPs and rejects invalid ones.
func TestMatchCIDR(t *testing.T) {
	t.Parallel()

	_, cidr, err := net.ParseCIDR("203.0.113.0/24")
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	if !matchCIDR("203.0.113.5", []*net.IPNet{cidr}) {
		t.Fatalf("expected IP to match CIDR")
	}
	if matchCIDR("198.51.100.1", []*net.IPNet{cidr}) {
		t.Fatalf("unexpected match for out-of-range IP")
	}
	if matchCIDR("not-an-ip", []*net.IPNet{cidr}) {
		t.Fatalf("expected invalid IP to miss")
	}
}

// TestMethodAllowed validates method gating logic based on configured allowlists.
func TestMethodAllowed(t *testing.T) {
	t.Parallel()

	allowed := map[string]struct{}{"GET": {}}
	if !methodAllowed("POST", allowed, false) {
		t.Fatalf("expected unrestricted methods to be allowed")
	}
	if !methodAllowed("get", allowed, true) {
		t.Fatalf("expected GET to be allowed when limited")
	}
	if methodAllowed("post", allowed, true) {
		t.Fatalf("expected POST to be rejected when limited")
	}
}

// TestEngineFinalizeGRPCIgnoreOKOnlyForcedMetrics verifies forced logging metrics for GRPC health checks.
func TestEngineFinalizeGRPCIgnoreOKOnlyForcedMetrics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		alwaysLog  bool
		wantForced uint64
	}{
		{
			name:       "ignoreStatusOnlyForcesLog",
			alwaysLog:  false,
			wantForced: 1,
		},
		{
			name:       "ignoreStatusAndAlwaysLogErrorsForceOnce",
			alwaysLog:  true,
			wantForced: 1,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			metrics := NewMetrics()
			engine := &Engine{
				metrics: metrics,
				cfg: Config{
					AlwaysLogErrors: tt.alwaysLog,
				},
			}
			decision := &Decision{
				Matched:            true,
				Protocol:           ProtocolGRPC,
				Reason:             ReasonGRPCHealth,
				Action:             ActionDrop,
				EffectiveAction:    ActionDrop,
				ignoreStatusOKOnly: true,
			}

			engine.FinalizeGRPC(decision, "UNKNOWN", 250*time.Millisecond, false)

			if !decision.ForceLog || decision.SafetyRail != SafetyRailError {
				t.Fatalf("expected decision to be forced with SafetyRailError: %+v", decision)
			}
			if decision.ActiveAction() != ActionMark {
				t.Fatalf("expected forced decision to downgrade to ActionMark, got %v", decision.ActiveAction())
			}
			forced := metrics.SnapshotForced()
			got := forced[forcedKey{Protocol: ProtocolGRPC, SafetyRail: SafetyRailError}]
			if got != tt.wantForced {
				t.Fatalf("forced metric count = %d, want %d", got, tt.wantForced)
			}
		})
	}
}

// TestEngineApplySamplingHTTP exercises sampling decisions for HTTP traffic under various settings.
func TestEngineApplySamplingHTTP(t *testing.T) {
	t.Parallel()

	engine := &Engine{
		now: func() time.Time {
			return time.Unix(0, 0)
		},
		sampling: sampler{
			httpKeepEvery:    1,
			httpMaxPerMinute: 0,
			random:           rand.New(rand.NewSource(1)),
		},
	}
	decision := &Decision{
		Matched:         true,
		Action:          ActionDrop,
		EffectiveAction: ActionDrop,
	}

	got := engine.applySampling(decision, ProtocolHTTP)
	if got != decision {
		t.Fatalf("applySampling returned new pointer; want same decision")
	}
	if decision.EffectiveAction != ActionMark || !decision.markOnly {
		t.Fatalf("applySampling did not convert to mark-only entry: %#v", decision)
	}

	// Second call with sampling disabled should leave the decision unchanged.
	engine.sampling.httpKeepEvery = 0
	decision.EffectiveAction = ActionDrop
	decision.markOnly = false
	engine.applySampling(decision, ProtocolHTTP)
	if decision.EffectiveAction != ActionDrop || decision.markOnly {
		t.Fatalf("unexpected sampling change with keepEvery=2: %#v", decision)
	}
}

// TestParseIP ensures parseIP extracts host components from IPv4 and IPv6 addresses.
func TestParseIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{input: "198.51.100.1:443", want: "198.51.100.1"},
		{input: "[2001:db8::1]:8443", want: "2001:db8::1"},
		{input: "203.0.113.5", want: "203.0.113.5"},
		{input: "", want: ""},
	}

	for _, tt := range tests {
		if got := parseIP(tt.input); got != tt.want {
			t.Fatalf("parseIP(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
