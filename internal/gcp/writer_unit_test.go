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
	"bytes"
	"errors"
	"io"
	"testing"
)

type closingBuffer struct {
	bytes.Buffer
	closed bool
	err    error
}

func (cb *closingBuffer) Close() error {
	cb.closed = true
	return cb.err
}

func TestNewSwitchableWriterDefaultsToDiscard(t *testing.T) {
	sw := NewSwitchableWriter(nil)
	n, err := sw.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("write returned error: %v", err)
	}
	if n != len("hello") {
		t.Fatalf("write length = %d, want %d", n, len("hello"))
	}

	if got := sw.GetCurrentWriter(); got == nil {
		t.Fatal("expected non-nil writer")
	}
}

func TestSwitchableWriterSetWriter(t *testing.T) {
	sw := NewSwitchableWriter(nil)

	var buf bytes.Buffer
	sw.SetWriter(&buf)
	if _, err := sw.Write([]byte("data")); err != nil {
		t.Fatalf("write returned error: %v", err)
	}
	if buf.String() != "data" {
		t.Fatalf("buffer contents = %q, want %q", buf.String(), "data")
	}

	sw.SetWriter(nil)
	if _, err := sw.Write([]byte("ignored")); err != nil {
		t.Fatalf("write to nil writer returned error: %v", err)
	}
	if buf.String() != "data" {
		t.Fatalf("buffer should remain unchanged, got %q", buf.String())
	}
}

func TestSwitchableWriterClose(t *testing.T) {
	cb := &closingBuffer{}
	sw := NewSwitchableWriter(cb)

	if err := sw.Close(); err != nil {
		t.Fatalf("close returned error: %v", err)
	}
	if !cb.closed {
		t.Fatal("expected underlying writer to be closed")
	}

	// Subsequent writes should go to io.Discard
	n, err := sw.Write([]byte("after-close"))
	if err != nil {
		t.Fatalf("write after close returned error: %v", err)
	}
	if n != len("after-close") {
		t.Fatalf("write length after close = %d, want %d", n, len("after-close"))
	}
}

func TestSwitchableWriterClosePropagatesError(t *testing.T) {
	cb := &closingBuffer{err: errors.New("boom")}
	sw := NewSwitchableWriter(cb)

	err := sw.Close()
	if !errors.Is(err, cb.err) {
		t.Fatalf("close error = %v, want %v", err, cb.err)
	}
}

func TestSwitchableWriterGetCurrentWriter(t *testing.T) {
	var buf bytes.Buffer
	sw := NewSwitchableWriter(&buf)

	if got := sw.GetCurrentWriter(); got != &buf {
		t.Fatalf("GetCurrentWriter returned %v, want %v", got, &buf)
	}

	sw.SetWriter(io.Discard)
	if got := sw.GetCurrentWriter(); got != io.Discard {
		t.Fatalf("GetCurrentWriter after SetWriter returned %v, want io.Discard", got)
	}
}
