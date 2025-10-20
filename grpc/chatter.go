package grpc

import (
	"log/slog"
	"strings"

	"github.com/pjscruggs/slogcp/healthcheck"
)

func appendChatterAnnotations(attrs []slog.Attr, decision *healthcheck.Decision, keys healthcheck.AuditKeys) []slog.Attr {
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
	if keys.Reason != "" && decision.Reason != healthcheck.ReasonUnknown {
		fieldAttrs = append(fieldAttrs, slog.String(keys.Reason, string(decision.Reason)))
	}
	if keys.Rule != "" && decision.Rule != "" {
		fieldAttrs = append(fieldAttrs, slog.String(keys.Rule, decision.Rule))
	}
	if keys.SafetyRail != "" && decision.SafetyRail != healthcheck.SafetyRailNone {
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

func buildChatterGroup(prefix string, attrs []slog.Attr) slog.Attr {
	segments := strings.Split(prefix, ".")
	lastIdx := len(segments) - 1
	lastKey := strings.TrimSpace(segments[lastIdx])
	if lastKey == "" {
		lastKey = prefix
	}
	group := slog.Attr{
		Key:   lastKey,
		Value: slog.GroupValue(attrs...),
	}
	for i := len(segments) - 2; i >= 0; i-- {
		name := strings.TrimSpace(segments[i])
		if name == "" {
			continue
		}
		group = slog.Attr{
			Key:   name,
			Value: slog.GroupValue(group),
		}
	}
	return group
}

func emitChatterConfig(logger *slog.Logger, engine *healthcheck.Engine) {
	if engine == nil || logger == nil {
		return
	}
	for _, warn := range engine.Warnings() {
		logger.Warn("Chatter reduction warning", slog.String("message", warn))
	}
}
