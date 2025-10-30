package chatter

import (
	"context"
	"maps"
	"strings"
	"time"
)

// Mode controls whether chatter reduction is active. When set to ModeAuto the
// engine relies on environment detection (e.g. metadata.OnGCE) to decide if
// suppression should be enabled by default.
type Mode string

const (
	// ModeAuto enables chatter reduction only when the environment detection
	// determines the process is running on Google Cloud.
	ModeAuto Mode = "auto"
	// ModeOn forces chatter reduction on regardless of the environment.
	ModeOn Mode = "on"
	// ModeOff disables chatter reduction entirely.
	ModeOff Mode = "off"
)

// Action describes what the engine should do when a chatter match is detected.
type Action string

const (
	// ActionDrop suppresses the log entry entirely unless a safety rail forces
	// the event to be kept.
	ActionDrop Action = "drop"
	// ActionMark keeps the log entry but annotates it with chatter metadata.
	ActionMark Action = "mark"
	// ActionRoute removes the entry from the primary sink and forwards it to an
	// alternate destination. Routing is optional; when it is not configured the
	// engine downgrades the action to ActionMark and records a warning.
	ActionRoute Action = "route"

	actionUnknown Action = ""
)

// SafetyRail indicates which mandatory logging rule caused a record to be
// emitted despite matching chatter suppression.
type SafetyRail string

const (
	// SafetyRailNone means the log entry was not forced by a safety rail.
	SafetyRailNone SafetyRail = ""
	// SafetyRailError forces logging when an HTTP status >= 400 or a non-OK
	// gRPC status code is observed.
	SafetyRailError SafetyRail = "error"
	// SafetyRailLatency forces logging when the request exceeded the configured
	// latency threshold.
	SafetyRailLatency SafetyRail = "latency"
)

// Reason enumerates the built-in chatter detection categories.
type Reason string

const (
	ReasonUnknown       Reason = ""
	ReasonLBIP          Reason = "lb_ip"
	ReasonGRPCHealth    Reason = "grpc_health"
	ReasonAppEngineAH   Reason = "appengine_ah"
	ReasonAppEngineCron Reason = "appengine_cron"
	ReasonPath          Reason = "path"
	ReasonPrefix        Reason = "prefix"
	ReasonRegex         Reason = "regex"
	ReasonHeader        Reason = "header"
	ReasonUserRule      Reason = "user_rule"
)

// Protocol identifies which transport the decision applies to.
type Protocol string

const (
	ProtocolUnknown Protocol = ""
	ProtocolHTTP    Protocol = "http"
	ProtocolGRPC    Protocol = "grpc"
)

// Decision captures the outcome of chatter detection for a single request.
// Instances are owned by a single request lifecycle and may be mutated by the
// engine when safety rails force a log to be emitted.
type Decision struct {
	Matched         bool
	Protocol        Protocol
	Action          Action
	EffectiveAction Action
	Reason          Reason
	Rule            string
	SafetyRail      SafetyRail
	ForceLog        bool

	// markOnly indicates the entry was kept purely for auditing (e.g. cron).
	markOnly bool
	// warnRouteFallback records when route mode had to be downgraded.
	warnRouteFallback bool
	// forcedSeverityHint holds the recommended severity override when the
	// latency safety rail fires (defaults to slog.LevelWarn, applied elsewhere).
	forcedSeverityHint string

	ignoreStatusOKOnly bool
}

// ActiveAction returns the action that should be applied after all safety rails
// run. When EffectiveAction is empty it falls back to Action.
func (d Decision) ActiveAction() Action {
	if d.EffectiveAction != actionUnknown {
		return d.EffectiveAction
	}
	return d.Action
}

// ShouldDrop reports whether logging should be suppressed.
func (d Decision) ShouldDrop() bool {
	return d.Matched && d.ActiveAction() == ActionDrop && !d.ForceLog
}

// ShouldMark reports whether the entry should be kept and annotated.
func (d Decision) ShouldMark() bool {
	return d.Matched && d.ActiveAction() == ActionMark
}

// ShouldRoute reports whether the entry should be diverted to a secondary sink.
func (d Decision) ShouldRoute() bool {
	return d.Matched && d.ActiveAction() == ActionRoute
}

// ShouldLog returns true when the log record must be emitted.
func (d Decision) ShouldLog() bool {
	if !d.Matched {
		return true
	}
	return d.ActiveAction() != ActionDrop || d.ForceLog
}

// SeverityHint returns an optional severity override indicator (e.g. WARN for latency rails).
func (d Decision) SeverityHint() string {
	return d.forcedSeverityHint
}

// MarkOnly reports whether the entry was kept purely for auditing.
func (d Decision) MarkOnly() bool {
	return d.markOnly
}

// RouteFallback reports whether route mode was requested but downgraded.
func (d Decision) RouteFallback() bool {
	return d.warnRouteFallback
}

// FilteredSummary captures the aggregate filtered count for a protocol/reason/action tuple.
type FilteredSummary struct {
	Protocol Protocol
	Reason   Reason
	Action   Action
	Count    uint64
}

// ForcedSummary captures the aggregate forced count for a protocol/safety rail tuple.
type ForcedSummary struct {
	Protocol   Protocol
	SafetyRail SafetyRail
	Count      uint64
}

// SummarySnapshot is the periodic aggregate emitted by the health check engine.
type SummarySnapshot struct {
	GeneratedAt      time.Time
	Filtered         []FilteredSummary
	Forced           []ForcedSummary
	ProxyFailures    uint64
	WatchTransitions map[string]uint64
}

// WatchEvent describes a health watch status update produced by the engine.
type WatchEvent struct {
	Peer           string
	Service        string
	Status         string
	PreviousStatus string
	Timestamp      time.Time
	ShouldLog      bool
	Suppressed     bool
	Transition     bool
	Level          string
}

// Clone returns a deep copy so callers can safely mutate the snapshot.
func (s SummarySnapshot) Clone() SummarySnapshot {
	out := SummarySnapshot{
		GeneratedAt:   s.GeneratedAt,
		ProxyFailures: s.ProxyFailures,
	}
	if len(s.Filtered) > 0 {
		out.Filtered = append([]FilteredSummary(nil), s.Filtered...)
	}
	if len(s.Forced) > 0 {
		out.Forced = append([]ForcedSummary(nil), s.Forced...)
	}
	if len(s.WatchTransitions) > 0 {
		out.WatchTransitions = make(map[string]uint64, len(s.WatchTransitions))
		maps.Copy(out.WatchTransitions, s.WatchTransitions)
	}
	return out
}

// WithSafetyRail returns a shallow copy of the decision with safety-rail
// metadata applied. The effective action automatically downgrades to mark so
// the log entry is emitted and annotated.
func (d Decision) WithSafetyRail(rail SafetyRail) Decision {
	if rail == SafetyRailNone {
		return d
	}
	d.SafetyRail = rail
	d.ForceLog = true
	if d.EffectiveAction == actionUnknown || d.EffectiveAction == ActionDrop || d.EffectiveAction == ActionRoute {
		d.EffectiveAction = ActionMark
	}
	return d
}

type contextKey struct{}

// ContextWithDecision stores the supplied decision pointer on the context.
func ContextWithDecision(ctx context.Context, decision *Decision) context.Context {
	if ctx == nil || decision == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, decision)
}

// DecisionFromContext retrieves a decision pointer from the context.
func DecisionFromContext(ctx context.Context) (*Decision, bool) {
	if ctx == nil {
		return nil, false
	}
	if decision, ok := ctx.Value(contextKey{}).(*Decision); ok && decision != nil {
		return decision, true
	}
	return nil, false
}

// sanitizeAction converts arbitrary strings into supported actions, returning
// the default when the input is unknown.
func sanitizeAction(raw string, fallback Action) Action {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "drop":
		return ActionDrop
	case "mark":
		return ActionMark
	case "route":
		return ActionRoute
	default:
		return fallback
	}
}
