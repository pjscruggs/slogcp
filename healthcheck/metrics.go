package healthcheck

import (
	"sync"
)

// Metrics tracks chatter reduction counters. It is safe for concurrent use.
type Metrics struct {
	mu        sync.Mutex
	filtered  map[filteredKey]uint64
	forced    map[forcedKey]uint64
	watch     map[string]uint64
	proxyFail uint64
}

type filteredKey struct {
	Protocol Protocol
	Reason   Reason
	Action   Action
}

type forcedKey struct {
	Protocol   Protocol
	SafetyRail SafetyRail
}

// NewMetrics constructs a metrics container.
func NewMetrics() *Metrics {
	return &Metrics{
		filtered: make(map[filteredKey]uint64),
		forced:   make(map[forcedKey]uint64),
		watch:    make(map[string]uint64),
	}
}

// IncFiltered increments the chatter filtered counter.
func (m *Metrics) IncFiltered(protocol Protocol, reason Reason, action Action) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := filteredKey{Protocol: protocol, Reason: reason, Action: action}
	m.filtered[key]++
}

// IncForced increments the safety-rail forced counter.
func (m *Metrics) IncForced(protocol Protocol, rail SafetyRail) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := forcedKey{Protocol: protocol, SafetyRail: rail}
	m.forced[key]++
}

// IncProxyTrustFailure records a proxy trust verification failure.
func (m *Metrics) IncProxyTrustFailure() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.proxyFail++
}

// IncWatchTransition increments watch transition counters.
func (m *Metrics) IncWatchTransition(status string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.watch[status]++
}

// SnapshotFiltered returns a copy of filtered counters.
func (m *Metrics) SnapshotFiltered() map[filteredKey]uint64 {
	out := make(map[filteredKey]uint64)
	if m == nil {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range m.filtered {
		out[k] = v
	}
	return out
}

// SnapshotForced returns a copy of forced counters.
func (m *Metrics) SnapshotForced() map[forcedKey]uint64 {
	out := make(map[forcedKey]uint64)
	if m == nil {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range m.forced {
		out[k] = v
	}
	return out
}

// SnapshotProxyFailures returns the current proxy trust failure count.
func (m *Metrics) SnapshotProxyFailures() uint64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.proxyFail
}

// SnapshotWatch transitions returns the watch counts.
func (m *Metrics) SnapshotWatch() map[string]uint64 {
	out := make(map[string]uint64)
	if m == nil {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range m.watch {
		out[k] = v
	}
	return out
}
