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

//go:build unit
// +build unit

package gcp

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

type stackErr struct {
	err error
	pcs []uintptr
}

func (e stackErr) Error() string { return e.err.Error() }

func (e stackErr) StackTrace() []uintptr { return e.pcs }

func captureTestPCs() []uintptr {
	const maxDepth = 16
	pcs := make([]uintptr, maxDepth)
	n := runtime.Callers(3, pcs) // skip runtime.Callers, captureTestPCs, and caller helper
	return pcs[:n]
}

func TestExtractAndFormatOriginStack(t *testing.T) {
	t.Helper()

	formatted := func() string {
		err := stackErr{
			err: errors.New("boom"),
			pcs: captureTestPCs(),
		}
		return extractAndFormatOriginStack(err)
	}()

	if formatted == "" {
		t.Fatalf("extractAndFormatOriginStack returned empty string")
	}
	if !strings.Contains(formatted, "TestExtractAndFormatOriginStack") {
		t.Fatalf("formatted stack %q does not contain test function name", formatted)
	}
}

func TestExtractAndFormatOriginStackNoStack(t *testing.T) {
	if got := extractAndFormatOriginStack(errors.New("plain")); got != "" {
		t.Fatalf("expected empty stack, got %q", got)
	}
}

func TestFormatPCsToStackStringSkipsGoexit(t *testing.T) {
	pcs := captureTestPCs()
	if len(pcs) == 0 {
		t.Fatal("captureTestPCs returned no frames")
	}

	stack := formatPCsToStackString(pcs)
	if stack == "" {
		t.Fatal("formatPCsToStackString returned empty string")
	}
	if strings.Contains(stack, "runtime.goexit") {
		t.Fatalf("formatted stack unexpectedly contains runtime.goexit: %q", stack)
	}
}
