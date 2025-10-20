package healthcheck

import "testing"

func TestMetricsCounters(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	if m == nil {
		t.Fatalf("NewMetrics returned nil")
	}

	m.IncFiltered(ProtocolHTTP, ReasonPath, ActionDrop)
	m.IncFiltered(ProtocolHTTP, ReasonPath, ActionDrop)
	m.IncForced(ProtocolHTTP, SafetyRailError)
	m.IncProxyTrustFailure()
	m.IncWatchTransition("NOT_SERVING")

	filterKey := filteredKey{Protocol: ProtocolHTTP, Reason: ReasonPath, Action: ActionDrop}
	if got := m.SnapshotFiltered()[filterKey]; got != 2 {
		t.Fatalf("SnapshotFiltered = %d, want 2", got)
	}

	forcedKey := forcedKey{Protocol: ProtocolHTTP, SafetyRail: SafetyRailError}
	if got := m.SnapshotForced()[forcedKey]; got != 1 {
		t.Fatalf("SnapshotForced = %d, want 1", got)
	}

	if got := m.SnapshotProxyFailures(); got != 1 {
		t.Fatalf("SnapshotProxyFailures = %d, want 1", got)
	}

	if got := m.SnapshotWatch()["NOT_SERVING"]; got != 1 {
		t.Fatalf("SnapshotWatch = %d, want 1", got)
	}
}
