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
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// Constants defining the maximum stack frames to capture for fallback traces.
const (
	maxStackFrames = 64
)

var stackPCPool = sync.Pool{
	New: func() any {
		buf := make([]uintptr, maxStackFrames)
		return &buf
	},
}

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
			// Limit the number of program counters to maxStackFrames
			if len(pcs) > maxStackFrames {
				pcs = pcs[:maxStackFrames]
			}
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

	header := currentGoroutineHeader()

	var sb strings.Builder
	if header != "" {
		sb.Grow(len(header) + len(pcs)*64)
		sb.WriteString(header)
		sb.WriteByte('\n')
	} else {
		sb.Grow(len(pcs) * 64)
	}

	var intBuf [20]byte
	frames := runtime.CallersFrames(pcs)
	frameCount := 0

	for {
		frame, more := frames.Next()

		if frame.PC == 0 {
			break
		}

		if frame.Function == "runtime.goexit" {
			if !more {
				break
			}
			continue
		}

		if frame.Function == "" {
			if !more {
				break
			}
			continue
		}

		sb.WriteString(frame.Function)
		sb.WriteByte('\n')
		sb.WriteByte('\t')
		sb.WriteString(frame.File)
		sb.WriteByte(':')

		lineBytes := strconv.AppendInt(intBuf[:0], int64(frame.Line), 10)
		sb.Write(lineBytes)

		if frame.PC != 0 && frame.Entry != 0 {
			var offset uintptr
			if frame.PC >= frame.Entry {
				offset = frame.PC - frame.Entry
			}
			if offset > 0 {
				sb.WriteString(" +0x")
				hexBytes := strconv.AppendUint(intBuf[:0], uint64(offset), 16)
				sb.Write(hexBytes)
			}
		}

		sb.WriteByte('\n')

		frameCount++
		if !more || frameCount >= maxStackFrames {
			break
		}
	}

	return sb.String()
}

// trimStackPCs removes leading frames that match skipFn while preserving the remainder.
func trimStackPCs(pcs []uintptr, skipFn func(string) bool) []uintptr {
	if len(pcs) == 0 {
		return pcs
	}

	frames := runtime.CallersFrames(pcs)
	skip := 0
	for {
		frame, more := frames.Next()
		if skipFn == nil || !skipFn(frame.Function) {
			break
		}
		skip++
		if !more {
			return nil
		}
	}
	if skip == 0 {
		return pcs
	}
	return pcs[skip:]
}

// SkipInternalStackFrame reports whether a stack frame belongs to slogcp or
// runtime internals and should be skipped when presenting stack traces to users.
func SkipInternalStackFrame(funcName string) bool {
	if funcName == "" {
		return false
	}
	switch funcName {
	case "runtime.Callers", "runtime.goexit":
		return true
	}
	if strings.HasPrefix(funcName, "runtime.") {
		return true
	}
	if strings.HasPrefix(funcName, "github.com/pjscruggs/slogcp/") ||
		strings.HasPrefix(funcName, "github.com/pjscruggs/slogcp.") ||
		strings.HasPrefix(funcName, "log/slog.") {
		return true
	}
	return false
}

// CaptureStack captures the current goroutine stack, trimming internal frames using skipFn
// (or SkipInternalStackFrame when nil). It returns the formatted stack trace string and
// the first remaining frame for use in report locations.
func CaptureStack(skipFn func(string) bool) (string, runtime.Frame) {
	bufPtr := stackPCPool.Get().(*[]uintptr)
	pcs := (*bufPtr)[:cap(*bufPtr)]

	n := runtime.Callers(0, pcs)
	if n == 0 {
		stackPCPool.Put(bufPtr)
		return "", runtime.Frame{}
	}
	pcs = pcs[:n]

	if skipFn == nil {
		skipFn = SkipInternalStackFrame
	}
	trimmed := trimStackPCs(pcs, skipFn)
	if len(trimmed) == 0 {
		trimmed = pcs
	}

	var top runtime.Frame
	if len(trimmed) > 0 {
		iter := runtime.CallersFrames(trimmed)
		top, _ = iter.Next()
	}

	stack := formatPCsToStackString(trimmed)
	stackPCPool.Put(bufPtr)
	return stack, top
}

// currentGoroutineHeader returns the goroutine header emitted by runtime.Stack.
func currentGoroutineHeader() string {
	const fallbackHeader = "goroutine 0 [running]:"

	var buf [128]byte
	n := runtime.Stack(buf[:], false)
	if n <= 0 {
		return fallbackHeader
	}

	header := string(buf[:n])
	if idx := strings.IndexByte(header, '\n'); idx >= 0 {
		header = header[:idx]
	}
	header = strings.TrimSuffix(header, "\r")
	header = strings.TrimSpace(header)
	if header == "" {
		return fallbackHeader
	}
	return header
}
