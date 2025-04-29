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
	"runtime"
	"strconv"
	"strings"
)

// stackTracer defines an interface errors can implement to provide their own stack trace
// in the form of program counters. This signature is compatible with several common
// error wrapping libraries that capture stack traces.
type stackTracer interface {
	StackTrace() []uintptr
}

// extractAndFormatOriginStack attempts to get a stack trace via the stackTracer interface
// implemented by the error (or one it wraps) and formats it.
//
// It returns the formatted stack trace string if the interface is found and provides
// program counters. Otherwise, it returns an empty string.
func extractAndFormatOriginStack(err error) string {
	// Strategy: Check if the error (or wrapped) implements stackTracer.
	currentErr := err
	for currentErr != nil {
		var st stackTracer
		// Use errors.As to check the current error in the chain.
		if errors.As(currentErr, &st) {
			pcs := st.StackTrace()
			if len(pcs) > 0 {
				// Format stack obtained from the error itself (origin stack).
				// No initial skipping needed for PCs provided by the error.
				formattedStack := formatPCsToStackString(pcs)
				if formattedStack != "" {
					return formattedStack // Return the origin stack trace.
				}
			}
		}
		// Move to the next error in the chain.
		currentErr = errors.Unwrap(currentErr)
	}

	// No stack trace found via the interface.
	return ""
}

// formatPCsToStackString formats program counters (pcs) into a standard Go stack trace string.
// It stops formatting frames once it encounters runtime exit frames.
func formatPCsToStackString(pcs []uintptr) string {
	if len(pcs) == 0 {
		return ""
	}

	var sb strings.Builder
	frames := runtime.CallersFrames(pcs)

	for {
		frame, more := frames.Next()

		if frame.PC == 0 {
			break // End of frames
		}

		funcName := frame.Function

		// Stop processing *before* formatting runtime exit frames.
		if funcName == "runtime.goexit" || strings.HasPrefix(funcName, "testing.") {
			break
		}

		// Format the relevant frame.
		sb.WriteString(frame.Function)
		sb.WriteByte('\n')
		sb.WriteByte('\t')
		sb.WriteString(frame.File)
		sb.WriteByte(':')
		sb.WriteString(strconv.Itoa(frame.Line))
		sb.WriteByte('\n')

		if !more {
			break
		}
	}
	// Ensure trailing newline is removed if present.
	return strings.TrimSuffix(sb.String(), "\n")
}
