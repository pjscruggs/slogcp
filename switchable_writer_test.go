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
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
)

// TestSwitchableWriterSetWriter verifies writes follow the current writer.
func TestSwitchableWriterSetWriter(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sw := NewSwitchableWriter(&buf)

	if _, err := sw.Write([]byte("first")); err != nil {
		t.Fatalf("Write returned %v", err)
	}
	if got := buf.String(); got != "first" {
		t.Fatalf("buffer = %q, want %q", got, "first")
	}

	sw.SetWriter(nil)
	if _, err := sw.Write([]byte("discarded")); err != nil {
		t.Fatalf("Write to nil writer returned %v", err)
	}

	var second bytes.Buffer
	sw.SetWriter(&second)
	if _, err := sw.Write([]byte("second")); err != nil {
		t.Fatalf("Write returned %v", err)
	}
	if got := second.String(); got != "second" {
		t.Fatalf("second buffer = %q, want %q", got, "second")
	}
}

// TestSwitchableWriterNilInitialWriter ensures nil initial writers default to io.Discard.
func TestSwitchableWriterNilInitialWriter(t *testing.T) {
	t.Parallel()

	sw := NewSwitchableWriter(nil)
	if got := sw.GetCurrentWriter(); got != io.Discard {
		t.Fatalf("GetCurrentWriter = %T, want io.Discard", got)
	}
	if _, err := sw.Write([]byte("discard")); err != nil {
		t.Fatalf("Write returned %v", err)
	}
}

// TestSwitchableWriterGetCurrentWriter tracks writer swaps through GetCurrentWriter.
func TestSwitchableWriterGetCurrentWriter(t *testing.T) {
	t.Parallel()

	var first bytes.Buffer
	var second bytes.Buffer

	sw := NewSwitchableWriter(&first)
	if got := sw.GetCurrentWriter(); got != &first {
		t.Fatalf("GetCurrentWriter = %v, want %v", got, &first)
	}

	sw.SetWriter(&second)
	if got := sw.GetCurrentWriter(); got != &second {
		t.Fatalf("GetCurrentWriter = %v, want %v", got, &second)
	}
}

// TestSwitchableWriterClose closes closable writers and guards repeated calls.
func TestSwitchableWriterClose(t *testing.T) {
	t.Parallel()

	writer := &closableBuffer{}
	sw := NewSwitchableWriter(writer)

	if err := sw.Close(); err != nil {
		t.Fatalf("Close returned %v", err)
	}
	if !writer.closed {
		t.Fatalf("underlying writer not closed")
	}

	if err := sw.Close(); err != nil {
		t.Fatalf("second Close returned %v", err)
	}
	if _, err := sw.Write([]byte("after-close")); err != nil {
		t.Fatalf("Write after Close returned %v", err)
	}
}

// TestSwitchableWriterWriteHandlesNil guards the os.ErrClosed branch used by zero-value writers.
func TestSwitchableWriterWriteHandlesNil(t *testing.T) {
	t.Parallel()

	var sw SwitchableWriter
	n, err := sw.Write([]byte("data"))
	if n != 0 {
		t.Fatalf("Write bytes = %d, want 0", n)
	}
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Write err = %v, want os.ErrClosed", err)
	}
}

// TestSwitchableWriterWriteWrapsError ensures write errors are wrapped and propagated.
func TestSwitchableWriterWriteWrapsError(t *testing.T) {
	t.Parallel()

	writeErr := errors.New("write-fail")
	sw := NewSwitchableWriter(errorWriter{err: writeErr})

	n, err := sw.Write([]byte("payload"))
	if n != 0 {
		t.Fatalf("Write bytes = %d, want 0", n)
	}
	if !errors.Is(err, writeErr) {
		t.Fatalf("Write error = %v, want write-fail", err)
	}
}

type closableBuffer struct {
	bytes.Buffer
	closed bool
}

// Close marks the buffer closed to verify SwitchableWriter.Close invokes it.
func (c *closableBuffer) Close() error {
	c.closed = true
	return nil
}

type errorWriter struct {
	err error
}

// Write returns the configured error to exercise error wrapping.
func (e errorWriter) Write([]byte) (int, error) {
	return 0, e.err
}
