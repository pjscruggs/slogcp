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

package gcp

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// TestExtractAndFormatOriginStack verifies stack trace extraction via the stackTracer interface.
func TestExtractAndFormatOriginStack(t *testing.T) {
	basicErr := errors.New("basic error")
	// Use the updated newStackError which captures PCs for the interface
	// The second arg ("stack string") is only for %+v formatting, not used by StackTrace()
	stackErr := newStackError("stack error", "")
	wrappedStackErr := fmt.Errorf("wrapped stack: %w", stackErr)
	wrappedBasicErr := fmt.Errorf("wrapped basic: %w", basicErr)
	doubleWrappedStackErr := fmt.Errorf("double wrap: %w", wrappedStackErr)

	testCases := []struct {
		name              string
		err               error
		wantStackNonEmpty bool   // Checks if origin stack was found
		wantStackContains string // Substring to check in origin stack (if non-empty)
	}{
		{
			name:              "Nil error",
			err:               nil,
			wantStackNonEmpty: false,
		},
		{
			name:              "Basic error (no stackTracer)",
			err:               basicErr,
			wantStackNonEmpty: false,
		},
		{
			name:              "Stack error (implements stackTracer)",
			err:               stackErr,
			wantStackNonEmpty: true,
			// Expect stack trace to contain the helper function that created the error
			wantStackContains: "newStackError",
		},
		{
			name:              "Wrapped stack error (inner implements stackTracer)",
			err:               wrappedStackErr,
			wantStackNonEmpty: true,
			// Expect stack trace from the inner error
			wantStackContains: "newStackError",
		},
		{
			name:              "Wrapped basic error (inner does not implement stackTracer)",
			err:               wrappedBasicErr,
			wantStackNonEmpty: false,
		},
		{
			name:              "Double wrapped stack error",
			err:               doubleWrappedStackErr,
			wantStackNonEmpty: true,
			// Expect stack trace from the innermost error implementing the interface
			wantStackContains: "newStackError",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gotOriginStackTrace := extractAndFormatOriginStack(tc.err)

			// Verify origin stack trace presence/absence.
			gotStackNonEmpty := gotOriginStackTrace != ""
			if gotStackNonEmpty != tc.wantStackNonEmpty {
				t.Errorf("Origin stack trace presence mismatch: got non-empty=%v, want non-empty=%v.\nStack:\n%s",
					gotStackNonEmpty, tc.wantStackNonEmpty, gotOriginStackTrace)
			}

			// If origin stack trace expected, perform basic content check.
			if tc.wantStackNonEmpty && tc.wantStackContains != "" {
				if !strings.Contains(gotOriginStackTrace, tc.wantStackContains) {
					t.Errorf("Origin stack trace mismatch: expected to contain %q, but got:\n%s",
						tc.wantStackContains, gotOriginStackTrace)
				}
			}
		})
	}
}

// helperFunctionForStackFormatting is used to create a known stack frame.
func helperFunctionForStackFormatting() []uintptr {
	// Capture all frames starting with skip=0
	allPCs := make([]uintptr, 10)
	n := runtime.Callers(0, allPCs)
	if n == 0 {
		return nil // No frames captured
	}

	// Get frames for filtering
	frames := runtime.CallersFrames(allPCs[:n])

	// Create a new slice for filtered PCs
	filteredPCs := make([]uintptr, 0, n)

	// Track if we've found this function in the stack
	var foundHelper bool

	for {
		frame, more := frames.Next()

		// Skip runtime frames
		if strings.HasPrefix(frame.Function, "runtime.") {
			if !more {
				break
			}
			continue
		}

		// Identify this helper function
		if !foundHelper && strings.HasSuffix(frame.Function, "helperFunctionForStackFormatting") {
			foundHelper = true
			if !more {
				break
			}
			continue
		}

		// After finding helper, capture all subsequent frames
		if foundHelper {
			filteredPCs = append(filteredPCs, frame.PC)
		}

		if !more {
			break
		}
	}

	return filteredPCs
}

// TestFormatPCsToStackString verifies the formatting of program counters.
func TestFormatPCsToStackString(t *testing.T) {
	t.Run("EmptyPCs", func(t *testing.T) {
		got := formatPCsToStackString(nil)
		if got != "" {
			t.Errorf("formatPCsToStackString(nil) = %q, want empty string", got)
		}
		got = formatPCsToStackString([]uintptr{})
		if got != "" {
			t.Errorf("formatPCsToStackString([]) = %q, want empty string", got)
		}
	})

	t.Run("ValidPCs", func(t *testing.T) {
		pcs := helperFunctionForStackFormatting()
		if len(pcs) == 0 {
			t.Fatal("Failed to capture PCs for test")
		}

		got := formatPCsToStackString(pcs)

		// Check for expected format: Function\n\tFile:Line\n...
		lines := strings.Split(got, "\n")
		if len(lines) < 2 {
			t.Fatalf("Expected at least 2 lines in stack trace, got %d:\n%s", len(lines), got)
		}

		// Check first frame (should be the caller of helperFunctionForStackFormatting, i.e., this test func)
		if !strings.Contains(lines[0], "TestFormatPCsToStackString") {
			t.Errorf("First line (function) %q does not contain expected test function name", lines[0])
		}
		if !strings.HasPrefix(lines[1], "\t") || !strings.Contains(lines[1], "stack_test.go:") {
			t.Errorf("Second line (file:line) %q does not match expected format '\\t...stack_test.go:NNN'", lines[1])
		}

		// Check that runtime/testing frames are excluded
		if strings.Contains(got, "runtime.goexit") {
			t.Errorf("Stack trace unexpectedly contains runtime.goexit:\n%s", got)
		}
		if strings.Contains(got, "testing.tRunner") {
			t.Errorf("Stack trace unexpectedly contains testing.tRunner:\n%s", got)
		}
	})

	t.Run("PCsIncludingGoexit", func(t *testing.T) {
		// Simulate PCs that might include runtime exit frames at the end
		pcs := make([]uintptr, 10)
		n := runtime.Callers(0, pcs) // Capture frames including runtime.Callers itself
		pcs = pcs[:n]

		got := formatPCsToStackString(pcs)

		// Verify that the output stops *before* runtime.goexit or testing frames
		if strings.Contains(got, "runtime.goexit") {
			t.Errorf("Stack trace including goexit was not truncated correctly:\n%s", got)
		}
		// Depending on test execution, testing.tRunner might appear earlier than goexit
		if strings.Contains(got, "testing.tRunner") {
			t.Errorf("Stack trace including tRunner was not truncated correctly:\n%s", got)
		}
		// Ensure *some* output was generated before truncation
		if got == "" && len(pcs) > 0 {
			t.Error("Expected some output before truncation, but got empty string")
		}
	})
}
