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
	"fmt"
	"io"
	"os"
	"sync"
)

// SwitchableWriter is an io.Writer that allows its underlying writer to be
// changed atomically. It is used internally when slogcp is configured to log to
// a specific file path (for example, via WithRedirectToFile) so that
// Handler.ReopenLogFile can update the output destination without rebuilding
// the entire handler chain.
//
// SwitchableWriter also implements io.Closer. Its Close method attempts to
// close the underlying writer if it implements io.Closer and then sets the
// internal writer to io.Discard to prevent further writes. The actual closing
// of file resources managed by slogcp is handled by the Handler itself.
type SwitchableWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewSwitchableWriter creates a new SwitchableWriter, initializing it with
// the provided initialWriter. If initialWriter is nil, the SwitchableWriter
// defaults to using io.Discard.
func NewSwitchableWriter(initialWriter io.Writer) *SwitchableWriter {
	if initialWriter == nil {
		initialWriter = io.Discard
	}
	return &SwitchableWriter{w: initialWriter}
}

// Write directs the given bytes to the current underlying writer.
// It is safe for concurrent use. If the SwitchableWriter has been closed or its
// writer set to nil, Write may return an error (such as os.ErrClosed).
func (sw *SwitchableWriter) Write(p []byte) (n int, err error) {
	sw.mu.Lock()
	currentWriter := sw.w
	if currentWriter == nil {
		sw.mu.Unlock()
		return 0, os.ErrClosed
	}
	n, err = currentWriter.Write(p)
	sw.mu.Unlock()
	if err != nil {
		return n, fmt.Errorf("write via switchable writer: %w", err)
	}
	return n, nil
}

// SetWriter atomically updates the underlying writer for this SwitchableWriter.
// The previously configured writer is not closed by this method; managing its
// lifecycle is the caller's responsibility. If newWriter is nil, the
// SwitchableWriter will subsequently direct writes to io.Discard.
func (sw *SwitchableWriter) SetWriter(newWriter io.Writer) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if newWriter == nil {
		sw.w = io.Discard
		return
	}
	sw.w = newWriter
}

// GetCurrentWriter returns the current underlying io.Writer that this
// SwitchableWriter is directing writes to. This method is safe for concurrent
// use but callers should avoid holding on to the returned writer across calls to
// SetWriter.
func (sw *SwitchableWriter) GetCurrentWriter() io.Writer {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.w
}

// Close attempts to close the current underlying writer if it implements
// io.Closer. After attempting to close the writer (or if it is not closable),
// the SwitchableWriter's internal writer is set to io.Discard to prevent
// further writes. This method is safe for concurrent use and idempotent.
func (sw *SwitchableWriter) Close() error {
	sw.mu.Lock()
	writerToClose := sw.w
	sw.w = io.Discard
	sw.mu.Unlock()

	if c, ok := writerToClose.(io.Closer); ok {
		if err := c.Close(); err != nil {
			return fmt.Errorf("close current writer: %w", err)
		}
	}
	return nil
}

// Ensure SwitchableWriter implements io.WriteCloser.
var _ io.WriteCloser = (*SwitchableWriter)(nil)
