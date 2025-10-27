package grpc

import (
	"log/slog"
	"strings"

	"github.com/pjscruggs/slogcp/chatter"
)

// appendChatterAnnotations attaches chatter-related attributes to the provided
// list when the decision matched a suppression rule.
func appendChatterAnnotations(attrs []slog.Attr, decision *chatter.Decision, keys chatter.AuditKeys) []slog.Attr {
	if decision == nil || !decision.Matched {
		return attrs
	}

	decisionValue := ""
	switch {
	case decision.MarkOnly():
		decisionValue = "would_suppress"
	case decision.ForceLog:
		decisionValue = "suppressed"
	default:
		decisionValue = "kept"
	}

	fieldAttrs := make([]slog.Attr, 0, 4)
	if keys.Decision != "" && decisionValue != "" {
		fieldAttrs = append(fieldAttrs, slog.String(keys.Decision, decisionValue))
	}
	if keys.Reason != "" && decision.Reason != chatter.ReasonUnknown {
		fieldAttrs = append(fieldAttrs, slog.String(keys.Reason, string(decision.Reason)))
	}
	if keys.Rule != "" && decision.Rule != "" {
		fieldAttrs = append(fieldAttrs, slog.String(keys.Rule, decision.Rule))
	}
	if keys.SafetyRail != "" && decision.SafetyRail != chatter.SafetyRailNone {
		fieldAttrs = append(fieldAttrs, slog.String(keys.SafetyRail, string(decision.SafetyRail)))
	}

	if len(fieldAttrs) == 0 {
		return attrs
	}
	if keys.Prefix == "" {
		return append(attrs, fieldAttrs...)
	}
	group := buildChatterGroup(keys.Prefix, fieldAttrs)
	return append(attrs, group)
}

// buildChatterGroup nests the chatter attributes under a dotted prefix,
// returning the top-level slog group attribute.
func buildChatterGroup(prefix string, attrs []slog.Attr) slog.Attr {
	segments := strings.Split(prefix, ".")
	lastIdx := len(segments) - 1
	lastKey := strings.TrimSpace(segments[lastIdx])
	if lastKey == "" {
		lastKey = prefix
	}
	group := slog.GroupAttrs(lastKey, attrs...)
	for i := len(segments) - 2; i >= 0; i-- {
		name := strings.TrimSpace(segments[i])
		if name == "" {
			continue
		}
		group = slog.GroupAttrs(name, group)
	}
	return group
}

// emitChatterConfig logs warnings emitted by the chatter engine about
// unsupported or downgraded configuration.
func emitChatterConfig(logger *slog.Logger, engine *chatter.Engine) {
	if engine == nil || logger == nil {
		return
	}
	for _, warn := range engine.Warnings() {
		logger.Warn("Chatter reduction warning", slog.String("message", warn))
	}
}
