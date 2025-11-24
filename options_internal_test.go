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
	"log/slog"
	"testing"
)

// TestWithAttrsSkipsEmptySlices ensures empty slices do not mutate options state.
func TestWithAttrsSkipsEmptySlices(t *testing.T) {
	t.Parallel()

	opt := WithAttrs(nil)
	opts := &options{}
	opt(opts)
	if len(opts.attrs) != 0 {
		t.Fatalf("opts.attrs length = %d, want 0", len(opts.attrs))
	}

	opt = WithAttrs([]slog.Attr{})
	opt(opts)
	if len(opts.attrs) != 0 {
		t.Fatalf("opts.attrs should remain empty for blank slices")
	}
}

// TestWithAttrsCopiesInput verifies the option stores its own copy of the provided attributes.
func TestWithAttrsCopiesInput(t *testing.T) {
	t.Parallel()

	input := []slog.Attr{slog.String("mutable", "before")}
	opts := &options{}
	WithAttrs(input)(opts)

	input[0] = slog.String("mutable", "after")

	if len(opts.attrs) != 1 || len(opts.attrs[0]) != 1 {
		t.Fatalf("opts.attrs not populated as expected: %#v", opts.attrs)
	}
	if got := opts.attrs[0][0].Value.String(); got != "before" {
		t.Fatalf("copied attribute = %q, want %q", got, "before")
	}
}
