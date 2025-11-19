package slogcp

import (
	"errors"
	"slices"
	"sync"
	"testing"
)

// TestErrorReportingAttrsUsesRuntimeContext verifies runtime service context data is applied.
func TestErrorReportingAttrsUsesRuntimeContext(t *testing.T) {
	t.Parallel()

	t.Cleanup(func() {
		runtimeInfoOnce = sync.Once{}
		runtimeInfo = RuntimeInfo{}
	})

	runtimeInfoOnce = sync.Once{}
	runtimeInfoOnce.Do(func() {
		runtimeInfo = RuntimeInfo{
			ServiceContext: map[string]string{
				"service": "runtime-svc",
				"version": "runtime-v2",
			},
		}
	})

	attrs := ErrorReportingAttrs(errors.New("kaboom"))
	if len(attrs) == 0 {
		t.Fatalf("ErrorReportingAttrs returned no attributes")
	}

	attrMap := make(map[string]any, len(attrs))
	for _, attr := range attrs {
		if attr.Key == "" {
			continue
		}
		attrMap[attr.Key] = attr.Value.Any()
	}

	serviceValue := attrMap["serviceContext"]
	ctx, ok := serviceValue.(map[string]any)
	if !ok {
		t.Fatalf("serviceContext type = %T, want map[string]any", serviceValue)
	}
	if ctx["service"] != "runtime-svc" || ctx["version"] != "runtime-v2" {
		t.Fatalf("serviceContext = %#v, want runtime service metadata", ctx)
	}

	if _, hasMessage := attrMap["message"]; hasMessage {
		t.Fatalf("unexpected message attribute when override not provided")
	}

	keys := make([]string, 0, len(attrMap))
	for k := range attrMap {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if _, ok := attrMap["stack_trace"]; !ok {
		t.Fatalf("stack_trace missing from attrs: %v", keys)
	}
}
