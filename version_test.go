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

package slogcp_test

import (
	"testing"

	"github.com/pjscruggs/slogcp"
)

// TestGetVersionReflectsVariable ensures GetVersion mirrors manual overrides.
func TestGetVersionReflectsVariable(t *testing.T) {
	t.Parallel()

	original := slogcp.Version
	slogcp.Version = "test-version"
	t.Cleanup(func() {
		slogcp.Version = original
	})

	if got := slogcp.GetVersion(); got != "test-version" {
		t.Fatalf("GetVersion() = %q, want %q", got, "test-version")
	}
}
