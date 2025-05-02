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
	"fmt"
	"runtime"
)

// common_test.go contains shared helper types and functions used across
// multiple test files within the gcp package.

// stackError is a helper error type implementing error, fmt.Formatter, and stackTracer
// for stack trace testing.
type stackError struct {
	msg   string
	stack string    // Pre-formatted stack trace for %+v (optional, for specific tests)
	pcs   []uintptr // Program counters for StackTrace()
}

// Error implements the error interface.
func (e *stackError) Error() string {
	return e.msg
}

// Format implements fmt.Formatter with %+v support to provide stack trace.
// This mimics how libraries like pkg/errors add stack traces when the %+v
// format specifier is used. If e.stack is empty, it behaves like default formatting.
func (e *stackError) Format(s fmt.State, verb rune) {
	if verb == 'v' && s.Flag('+') && e.stack != "" {
		_, _ = fmt.Fprint(s, e.msg)
		_, _ = fmt.Fprint(s, "\n"+e.stack) // Append pre-formatted stack if available
		return
	}
	// Default formatting just prints the message.
	_, _ = fmt.Fprint(s, e.msg)
}

// newStackError is a constructor for the stackError helper type.
// The 'stack' parameter is optional and primarily for testing the %+v formatting;
// the StackTrace() method relies on captured PCs.
func newStackError(msg, stack string) error {
	// Capture all frames including runtime.Callers and this function
	pcs := make([]uintptr, maxStackFrames)
	n := runtime.Callers(0, pcs) // Skip 0 to get all frames

	if n == 0 {
		// No frames captured, return error with empty stack
		return &stackError{
			msg:   msg,
			stack: stack,
			pcs:   []uintptr{},
		}
	}

	// Get frames for filtering
	frames := runtime.CallersFrames(pcs[:n])

	// Create new slice for filtered PCs
	filteredPCs := make([]uintptr, 0, n)

	// Add all frames to filteredPCs except runtime.Callers
	// but ensure newStackError is included
	for {
		frame, more := frames.Next()

		// Skip only runtime.Callers frame
		if frame.Function == "runtime.Callers" {
			if !more {
				break
			}
			continue
		}

		// Add this frame's PC
		filteredPCs = append(filteredPCs, frame.PC)

		if !more {
			break
		}
	}

	return &stackError{
		msg:   msg,
		stack: stack,       // For testing %+v formatting (optional)
		pcs:   filteredPCs, // For StackTrace() interface, now with newStackError included
	}
}

// StackTrace implements the stackTracer interface.
// It returns the program counters captured during creation.
func (e *stackError) StackTrace() []uintptr {
	// Return the stored program counters. If they weren't captured correctly
	// during creation (e.g., n=0 from runtime.Callers), this will return nil/empty.
	return e.pcs
}

// Compile-time check that stackError implements stackTracer.
// This requires stackTracer to be defined (it's in stack.go).
var _ stackTracer = (*stackError)(nil)

// Compile-time check that stackError implements fmt.Formatter.
var _ fmt.Formatter = (*stackError)(nil)
