// Copyright 2025-2026 Patrick J. Scruggs
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

package main

import (
	"bytes"
	"context"
	"testing"
)

func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf); err != nil {
		t.Fatalf("run returned error: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatalf("expected log output, got none")
	}
}

func BenchmarkRun(b *testing.B) {
	ctx := context.Background()
	scenario, err := newClientScenario(ctx)
	if err != nil {
		b.Fatalf("new client scenario: %v", err)
	}
	defer scenario.Close()

	b.ResetTimer()
	for b.Loop() {
		var buf bytes.Buffer
		if err := scenario.Run(ctx, &buf); err != nil {
			b.Fatalf("scenario run returned error: %v", err)
		}
	}
}
