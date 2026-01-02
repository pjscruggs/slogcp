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

package slogcp

import (
	"errors"
	"reflect"
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

type frameIterator interface {
	Next() (runtime.Frame, bool)
}

var (
	runtimeCallersFunc = runtime.Callers
	runtimeStackFunc   = runtime.Stack
	callersFramesFunc  = func(pcs []uintptr) frameIterator {
		return runtime.CallersFrames(pcs)
	}
	goroutineHeaderFunc = defaultGoroutineHeader
)

// stackTracer defines an interface errors can implement to provide their own stack trace
// in the form of program counters.
type stackTracer interface {
	StackTrace() []uintptr
}

// extractAndFormatOriginStack attempts to get a stack trace via stack tracer interfaces
// (including pkg/errors-style StackTrace methods) and formats it according to Go standards.
// It returns an empty string if the interface is not found or provides no PCs.
func extractAndFormatOriginStack(err error) string {
	pcs := stackPCsFromError(err)
	if len(pcs) == 0 {
		return ""
	}
	if len(pcs) > maxStackFrames {
		pcs = pcs[:maxStackFrames]
	}
	return formatPCsToStackString(pcs)
}

var uintptrType = reflect.TypeOf(uintptr(0))

// stackPCsFromError extracts program counters from supported stack-trace methods.
func stackPCsFromError(err error) []uintptr {
	if err == nil {
		return nil
	}

	var st stackTracer
	if errors.As(err, &st) {
		if pcs := st.StackTrace(); len(pcs) > 0 {
			return pcs
		}
	}

	for e := err; e != nil; e = errors.Unwrap(e) {
		if pcs := stackPCsFromStackTraceMethod(e); len(pcs) > 0 {
			return pcs
		}
	}
	return nil
}

// stackPCsFromStackTraceMethod uses reflection to read stack traces from methods that
// return slices of uintptr-compatible values (for example pkg/errors.StackTrace).
func stackPCsFromStackTraceMethod(err error) []uintptr {
	if err == nil {
		return nil
	}
	value := reflect.ValueOf(err)
	method := value.MethodByName("StackTrace")
	if !method.IsValid() {
		return nil
	}
	if method.Type().NumIn() != 0 || method.Type().NumOut() != 1 {
		return nil
	}
	out := method.Call(nil)
	return stackPCsFromValue(out[0])
}

// stackPCsFromValue converts a slice/array of uintptr-compatible values into []uintptr.
func stackPCsFromValue(value reflect.Value) []uintptr {
	normalized, ok := normalizeStackValue(value)
	if !ok {
		return nil
	}
	return stackPCsFromSlice(normalized)
}

// normalizeStackValue ensures value is a non-empty slice/array, dereferencing pointers.
func normalizeStackValue(value reflect.Value) (reflect.Value, bool) {
	if !value.IsValid() {
		return reflect.Value{}, false
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return reflect.Value{}, false
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Slice && value.Kind() != reflect.Array {
		return reflect.Value{}, false
	}
	if value.Len() == 0 {
		return reflect.Value{}, false
	}
	return value, true
}

// stackPCsFromSlice converts slice or array elements into program counters.
func stackPCsFromSlice(value reflect.Value) []uintptr {
	pcs := make([]uintptr, 0, value.Len())
	for i := 0; i < value.Len(); i++ {
		elem := value.Index(i)
		if elem.Kind() == reflect.Pointer {
			if elem.IsNil() {
				continue
			}
			elem = elem.Elem()
		}
		if !elem.IsValid() || !elem.Type().ConvertibleTo(uintptrType) {
			return nil
		}
		pcs = append(pcs, elem.Convert(uintptrType).Interface().(uintptr))
	}
	if len(pcs) == 0 {
		return nil
	}
	return pcs
}

// formatPCsToStackString formats program counters (pcs) into a standard Go stack trace string,
// suitable for inclusion in logs and recognized by Cloud Error Reporting.
// It skips runtime exit frames.
func formatPCsToStackString(pcs []uintptr) string {
	if len(pcs) == 0 {
		return ""
	}

	header := currentGoroutineHeader()
	sb := initStackBuilder(header, len(pcs))

	var intBuf [20]byte
	frames := callersFramesFunc(pcs)
	frameCount := 0

	for {
		frame, more := frames.Next()

		if frameShouldStop(frame) {
			break
		}

		if skip, stop := frameSkipState(frame, more); stop {
			break
		} else if skip {
			continue
		}

		appendFrame(sb, frame, &intBuf)

		frameCount++
		if !more || frameCount >= maxStackFrames {
			break
		}
	}

	return sb.String()
}

// initStackBuilder prepares a strings.Builder sized for the expected stack output.
func initStackBuilder(header string, pcsLen int) *strings.Builder {
	sb := &strings.Builder{}
	if header != "" {
		sb.Grow(len(header) + pcsLen*64)
		sb.WriteString(header)
		sb.WriteByte('\n')
		return sb
	}
	sb.Grow(pcsLen * 64)
	return sb
}

// frameShouldStop reports whether stack iteration should halt for an empty frame.
func frameShouldStop(frame runtime.Frame) bool {
	return frame.PC == 0
}

// frameSkipState returns whether to skip the frame and whether iteration should end.
func frameSkipState(frame runtime.Frame, more bool) (skip bool, stop bool) {
	switch frame.Function {
	case "runtime.goexit":
		return true, !more
	case "":
		return true, !more
	default:
		return false, false
	}
}

// appendFrame writes a single stack frame to the builder.
func appendFrame(sb *strings.Builder, frame runtime.Frame, intBuf *[20]byte) {
	sb.WriteString(frame.Function)
	sb.WriteByte('\n')
	sb.WriteByte('\t')
	sb.WriteString(frame.File)
	sb.WriteByte(':')

	lineBytes := strconv.AppendInt(intBuf[:0], int64(frame.Line), 10)
	sb.Write(lineBytes)

	appendOffset(sb, frame, intBuf)
	sb.WriteByte('\n')
}

// appendOffset renders the PC offset when available.
func appendOffset(sb *strings.Builder, frame runtime.Frame, intBuf *[20]byte) {
	if frame.PC == 0 || frame.Entry == 0 || frame.PC < frame.Entry {
		return
	}
	offset := frame.PC - frame.Entry
	if offset == 0 {
		return
	}
	sb.WriteString(" +0x")
	hexBytes := strconv.AppendUint(intBuf[:0], uint64(offset), 16)
	sb.Write(hexBytes)
}

// trimStackPCs removes leading frames that match skipFn while preserving the remainder.
func trimStackPCs(pcs []uintptr, skipFn func(string) bool) []uintptr {
	if len(pcs) == 0 {
		return pcs
	}

	frames := callersFramesFunc(pcs)
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

var (
	internalStackFrameNames = map[string]struct{}{
		"runtime.Callers": {},
		"runtime.goexit":  {},
	}
	internalStackFramePrefixes = []string{
		"runtime.",
		"github.com/pjscruggs/slogcp/",
		"github.com/pjscruggs/slogcp.",
		"log/slog.",
	}
)

// SkipInternalStackFrame reports whether funcName refers to a frame that
// belongs to slogcp or runtime internals and should be hidden from user-facing
// stack traces.
func SkipInternalStackFrame(funcName string) bool {
	if funcName == "" {
		return false
	}
	if _, found := internalStackFrameNames[funcName]; found {
		return true
	}
	for _, prefix := range internalStackFramePrefixes {
		if strings.HasPrefix(funcName, prefix) {
			return true
		}
	}
	return false
}

// CaptureStack captures the current goroutine stack, trimming internal frames using skipFn
// (or SkipInternalStackFrame when nil). It returns the formatted stack trace string and
// the first remaining frame for use in report locations.
func CaptureStack(skipFn func(string) bool) (string, runtime.Frame) {
	bufPtr := stackPCPool.Get().(*[]uintptr)
	pcs := (*bufPtr)[:cap(*bufPtr)]

	n := runtimeCallersFunc(0, pcs)
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
		iter := callersFramesFunc(trimmed)
		top, _ = iter.Next()
	}

	stack := formatPCsToStackString(trimmed)
	stackPCPool.Put(bufPtr)
	return stack, top
}

// currentGoroutineHeader returns the goroutine header emitted by runtime.Stack.
func currentGoroutineHeader() string {
	return goroutineHeaderFunc()
}

// defaultGoroutineHeader retrieves and normalizes the active goroutine header from runtime.Stack.
func defaultGoroutineHeader() string {
	const fallbackHeader = "goroutine 0 [running]:"

	var buf [128]byte
	n := runtimeStackFunc(buf[:], false)
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
