package slogcp

import (
	"bytes"
	"testing"
)

// TestSwitchableWriterSetAndGet exercises writer swapping and GetCurrentWriter coverage paths.
func TestSwitchableWriterSetAndGet(t *testing.T) {
	t.Parallel()

	first := &bytes.Buffer{}
	sw := NewSwitchableWriter(first)

	if got := sw.GetCurrentWriter(); got != first {
		t.Fatalf("initial writer = %v, want %v", got, first)
	}

	second := &bytes.Buffer{}
	sw.SetWriter(second)

	if _, err := sw.Write([]byte("hello")); err != nil {
		t.Fatalf("Write returned %v, want nil", err)
	}
	if second.String() != "hello" {
		t.Fatalf("second writer captured %q, want %q", second.String(), "hello")
	}

	sw.SetWriter(nil)
	if _, err := sw.Write([]byte("discarded")); err != nil {
		t.Fatalf("Write after nil SetWriter returned %v, want nil", err)
	}
	if got := sw.GetCurrentWriter(); got == nil {
		t.Fatalf("GetCurrentWriter returned nil after SetWriter(nil)")
	}
}

// TestSwitchableWriterClose closes an owned writer and routes subsequent writes to io.Discard.
func TestSwitchableWriterClose(t *testing.T) {
	t.Parallel()

	closer := &closableBuffer{}
	sw := NewSwitchableWriter(closer)

	if err := sw.Close(); err != nil {
		t.Fatalf("Close returned %v, want nil", err)
	}
	if !closer.closed {
		t.Fatalf("underlying writer was not closed")
	}

	if n, err := sw.Write([]byte("after-close")); err != nil || n != len("after-close") {
		t.Fatalf("Write after Close = (%d, %v), want (%d, nil)", n, err, len("after-close"))
	}
}

type closableBuffer struct {
	bytes.Buffer
	closed bool
}

// Close marks the buffer closed for verification.
func (c *closableBuffer) Close() error {
	c.closed = true
	return nil
}
