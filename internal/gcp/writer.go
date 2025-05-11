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
	"io"
	"os"
	"sync"
)

// SwitchableWriter is an io.Writer that allows its underlying writer to be
// changed atomically. It is used internally by the slogcp library's
// gcpHandler when slogcp is configured to log to a specific file path,
// for example, when using the slogcp.WithRedirectToFile option.
//
// This design enables the slogcp.Logger.ReopenLogFile method to update the
// handler's output destination (e.g., after an external log rotation event)
// without needing to reconstruct the entire logging handler chain.
//
// SwitchableWriter also implements io.Closer. Its Close method attempts to
// close the underlying writer if it implements io.Closer and then sets the
// internal writer to io.Discard to prevent further writes. The actual closing
// of file resources managed by slogcp is handled by the slogcp.Logger.
type SwitchableWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewSwitchableWriter creates a new SwitchableWriter, initializing it with
// the provided initialWriter.
// If initialWriter is nil, the SwitchableWriter defaults to using io.Discard
// for its underlying writer to prevent nil pointer dereferences during
// subsequent Write calls.
func NewSwitchableWriter(initialWriter io.Writer) *SwitchableWriter {
	if initialWriter == nil {
		initialWriter = io.Discard
	}
	return &SwitchableWriter{w: initialWriter}
}

// Write directs the given bytes to the current underlying writer.
// It is safe for concurrent use. If the SwitchableWriter has been closed
// or its writer set to nil (or io.Discard), Write may return an error
// (such as os.ErrClosed) or write to io.Discard.
func (sw *SwitchableWriter) Write(p []byte) (n int, err error) {
	sw.mu.Lock()
	currentWriter := sw.w
	sw.mu.Unlock()

	if currentWriter == nil {
		return 0, os.ErrClosed
	}
	return currentWriter.Write(p)
}

// SetWriter atomically updates the underlying writer for this SwitchableWriter.
// The previously configured writer is NOT closed by this method; managing the
// lifecycle of the old writer is the responsibility of the caller if needed.
// This method is safe for concurrent use.
// If newWriter is nil, the SwitchableWriter will subsequently direct writes
// to io.Discard.
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
// SwitchableWriter is directing writes to.
// This method is safe for concurrent use.
// The returned writer should generally not be stored or used for extended
// periods if SetWriter might be called concurrently, as the underlying
// writer could change. It is primarily intended for internal uses, such as
// attempting to flush the actual target writer.
func (sw *SwitchableWriter) GetCurrentWriter() io.Writer {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.w
}

// Close attempts to close the current underlying writer if it implements
// the io.Closer interface. After attempting to close the underlying writer,
// or if the underlying writer is not closable, the SwitchableWriter's
// internal writer is set to io.Discard to prevent further writes to the
// (potentially closed or replaced) original writer.
// This method is safe for concurrent use and is idempotent regarding the
// state of the SwitchableWriter (i.e., subsequent calls will operate on
// io.Discard).
func (sw *SwitchableWriter) Close() error {
	sw.mu.Lock()
	writerToClose := sw.w
	sw.w = io.Discard // Ensure future writes go to discard after Close is called.
	sw.mu.Unlock()

	if c, ok := writerToClose.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// Ensure SwitchableWriter implements io.WriteCloser.
var _ io.WriteCloser = (*SwitchableWriter)(nil)