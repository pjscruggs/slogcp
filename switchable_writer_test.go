package slogcp

import (
	"bytes"
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

type closableBuffer struct {
	bytes.Buffer
	closed bool
}

// Close marks the buffer closed to verify SwitchableWriter.Close invokes it.
func (c *closableBuffer) Close() error {
	c.closed = true
	return nil
}
