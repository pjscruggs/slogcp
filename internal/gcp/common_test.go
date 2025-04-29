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

// stackError is a helper error type implementing fmt.Formatter and stackTracer
// for stack trace testing.
type stackError struct {
	msg   string
	stack string    // Pre-formatted stack trace for %+v
	pcs   []uintptr // Program counters for StackTrace()
}

// Error implements the error interface.
func (e *stackError) Error() string {
	return e.msg
}

// Format implements fmt.Formatter with %+v support to provide stack trace.
// This mimics how libraries like pkg/errors add stack traces when the %+v
// format specifier is used.
func (e *stackError) Format(s fmt.State, verb rune) {
	if verb == 'v' && s.Flag('+') {
		fmt.Fprint(s, e.msg)
		if e.stack != "" {
			// Append stack trace with a newline separator.
			fmt.Fprint(s, "\n"+e.stack)
		}
		return
	}
	// Default formatting just prints the message.
	fmt.Fprint(s, e.msg)
}

// newStackError is a constructor for the stackError helper type.
// It captures program counters automatically for the StackTrace method.
func newStackError(msg, stack string) error {
	// Capture program counters for the StackTrace() method.
	// Skip runtime.Callers, this function (newStackError).
	pcs := make([]uintptr, maxStackFrames) // Use maxStackFrames from payload.go
	n := runtime.Callers(1, pcs)

	return &stackError{
		msg:   msg,
		stack: stack,   // For %+v formatting
		pcs:   pcs[:n], // For StackTrace() interface
	}
}

// StackTrace implements the stackTracer interface.
func (e *stackError) StackTrace() []uintptr {
	// Return the stored program counters, or capture if nil
	if e.pcs == nil {
		// Capture a minimal stack trace if none was provided during creation.
		// Skip runtime.Callers, and this function (StackTrace).
		pcs := make([]uintptr, 10)
		n := runtime.Callers(2, pcs)
		e.pcs = pcs[:n]
	}
	return e.pcs
}
