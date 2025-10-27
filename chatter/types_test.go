package chatter

import (
	"context"
	"testing"
	"time"
)

func TestDecisionHelpers(t *testing.T) {
	t.Parallel()

	base := Decision{
		Matched:         true,
		Action:          ActionDrop,
		EffectiveAction: actionUnknown,
	}

	if !base.ShouldDrop() || base.ShouldMark() || base.ShouldRoute() || base.ShouldLog() {
		t.Fatalf("unexpected helper behaviour for drop decision: %#v", base)
	}

	marked := Decision{
		Matched:         true,
		Action:          ActionMark,
		EffectiveAction: ActionMark,
	}
	if !marked.ShouldLog() || !marked.ShouldMark() || marked.ShouldDrop() {
		t.Fatalf("mark decision helper mismatch: %#v", marked)
	}

	routed := Decision{
		Matched:           true,
		Action:            ActionRoute,
		EffectiveAction:   ActionRoute,
		warnRouteFallback: false,
	}
	if !routed.ShouldRoute() {
		t.Fatalf("route decision should request routing: %#v", routed)
	}
}

func TestDecisionWithSafetyRail(t *testing.T) {
	t.Parallel()

	decision := Decision{
		Matched:         true,
		Action:          ActionDrop,
		EffectiveAction: actionUnknown,
	}

	updated := decision.WithSafetyRail(SafetyRailError)
	if !updated.ForceLog || updated.SafetyRail != SafetyRailError {
		t.Fatalf("safety rail not applied: %#v", updated)
	}
	if updated.EffectiveAction != ActionMark {
		t.Fatalf("expected downgrade to ActionMark when safety rail fires: %#v", updated)
	}

	noop := decision.WithSafetyRail(SafetyRailNone)
	if noop != decision {
		t.Fatalf("expected no-op when rail is SafetyRailNone")
	}
}

func TestDecisionContextRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	decision := &Decision{Rule: "test"}

	if out, ok := DecisionFromContext(ctx); ok || out != nil {
		t.Fatalf("expected empty context to yield no decision")
	}

	ctx = ContextWithDecision(ctx, decision)
	out, ok := DecisionFromContext(ctx)
	if !ok || out != decision {
		t.Fatalf("expected context round-trip to return same pointer")
	}
}

func TestSanitizeAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		fallback Action
		want     Action
	}{
		{input: "drop", fallback: ActionMark, want: ActionDrop},
		{input: "MARK", fallback: ActionDrop, want: ActionMark},
		{input: " route ", fallback: ActionDrop, want: ActionRoute},
		{input: "unknown", fallback: ActionMark, want: ActionMark},
	}

	for _, tt := range tests {
		if got := sanitizeAction(tt.input, tt.fallback); got != tt.want {
			t.Fatalf("sanitizeAction(%q, %v) = %v, want %v", tt.input, tt.fallback, got, tt.want)
		}
	}
}

func TestSummarySnapshotClone(t *testing.T) {
	t.Parallel()

	snapshot := SummarySnapshot{
		GeneratedAt: time.Unix(100, 0),
		Filtered: []FilteredSummary{
			{Protocol: ProtocolHTTP, Reason: ReasonPath, Action: ActionDrop, Count: 5},
		},
		Forced: []ForcedSummary{
			{Protocol: ProtocolHTTP, SafetyRail: SafetyRailError, Count: 3},
		},
		ProxyFailures: 7,
		WatchTransitions: map[string]uint64{
			"NOT_SERVING": 2,
		},
	}

	clone := snapshot.Clone()
	if &clone == &snapshot {
		t.Fatalf("Clone returned same struct pointer")
	}
	if clone.GeneratedAt != snapshot.GeneratedAt || clone.ProxyFailures != snapshot.ProxyFailures {
		t.Fatalf("clone lost scalar fields: %#v", clone)
	}

	clone.Filtered[0].Count = 42
	clone.Forced[0].Count = 99
	clone.WatchTransitions["NOT_SERVING"] = 5

	if snapshot.Filtered[0].Count == 42 || snapshot.Forced[0].Count == 99 || snapshot.WatchTransitions["NOT_SERVING"] == 5 {
		t.Fatalf("mutating clone affected original snapshot")
	}
}
