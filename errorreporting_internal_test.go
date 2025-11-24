// Copyright 2025 Patrick J. Scruggs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package slogcp

import (
	"errors"
	"slices"
	"testing"
)

// TestErrorReportingAttrsUsesRuntimeContext verifies runtime service context data is applied.
func TestErrorReportingAttrsUsesRuntimeContext(t *testing.T) {
	t.Parallel()

	t.Cleanup(resetRuntimeInfoCache)

	stubRuntimeInfo(RuntimeInfo{
		ServiceContext: map[string]string{
			"service": "runtime-svc",
			"version": "runtime-v2",
		},
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
