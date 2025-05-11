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
// in the form of program counters. Compatible with github.com/pkg/errors.
type stackTracer interface {
	StackTrace() []uintptr
}

// extractAndFormatOriginStack attempts to get a stack trace via the stackTracer interface
// implemented by the error (or one it wraps) and formats it according to Go standards.
// It returns an empty string if the interface is not found or provides no PCs.
func extractAndFormatOriginStack(err error) string {
	var st stackTracer
	// Use errors.As to find the first error in the chain implementing the interface.
	if errors.As(err, &st) {
		pcs := st.StackTrace()
		if len(pcs) > 0 {
			// Format the program counters obtained from the error.
			return formatPCsToStackString(pcs)
		}
	}
	return "" // No stack trace available from the error itself.
}

// formatPCsToStackString formats program counters (pcs) into a standard Go stack trace string,
// suitable for inclusion in logs and recognized by Cloud Error Reporting.
// It skips runtime exit frames.
func formatPCsToStackString(pcs []uintptr) string {
	if len(pcs) == 0 {
		return ""
	}

	var sb strings.Builder
	frames := runtime.CallersFrames(pcs)

	for {
		frame, more := frames.Next()

		// Basic frame validity check.
		if frame.PC == 0 {
			break // Should not happen with valid pcs, but safeguard.
		}

		// Skip runtime finalizers or exit points.
		if frame.Function == "runtime.goexit" {
			if !more {
				break
			} // Stop if it's the last frame
			continue // Skip this frame and continue if more exist
		}

		// Format the frame in standard Go stack trace format.
		sb.WriteString(frame.Function)
		sb.WriteByte('\n')
		sb.WriteByte('\t')
		sb.WriteString(frame.File)
		sb.WriteByte(':')
		sb.WriteString(strconv.Itoa(frame.Line))
		sb.WriteByte('\n')

		if !more {
			break // No more frames to process.
		}
	}

	// Return the built string, removing the final newline.
	return strings.TrimSuffix(sb.String(), "\n")
}
