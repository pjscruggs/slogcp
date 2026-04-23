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
	"testing"
	"time"
)

func TestHarnessTimeoutDefaultUsesEnvironmentSeconds(t *testing.T) {
	t.Setenv("E2E_TIMEOUT_SECONDS", "5400")

	got, err := harnessTimeoutDefault()
	if err != nil {
		t.Fatalf("harnessTimeoutDefault() error = %v", err)
	}

	if got != 90*time.Minute {
		t.Fatalf("harnessTimeoutDefault() = %v, want %v", got, 90*time.Minute)
	}
}

func TestHarnessTimeoutDefaultRejectsInvalidEnvironmentSeconds(t *testing.T) {
	t.Setenv("E2E_TIMEOUT_SECONDS", "25m")

	if _, err := harnessTimeoutDefault(); err == nil {
		t.Fatal("harnessTimeoutDefault() error = nil, want error")
	}
}
