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

package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/pjscruggs/slogcp/slogcpasync"
)

// TestAsyncExampleDropsLateWrite ensures the drop handler observes records
// logged after the async handler has been closed.
func TestAsyncExampleDropsLateWrite(t *testing.T) {
	t.Parallel()

	writer := newBlockingWriter()
	example, err := newAsyncExample(writer)
	if err != nil {
		t.Fatalf("newAsyncExample: %v", err)
	}

	time.Sleep(5 * time.Millisecond) // allow worker goroutine to start
	if _, ok := example.handler.Handler.(*slogcpasync.Handler); !ok {
		t.Fatalf("expected async handler, got %T", example.handler.Handler)
	}
	example.logger.Info("first")

	writer.release()

	if err := example.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	example.logger.Info("second") // dropped after Close

	waitForDrops(t, example.drops, 1)

	entries := writer.entries(t)
	if len(entries) != 1 || entries[0]["message"] != "first" {
		t.Fatalf("entries = %v, want first message only", entries)
	}

	if msgs := example.drops.messagesSnapshot(); len(msgs) != 1 || msgs[0] != "second" {
		t.Fatalf("dropped messages = %v, want [second]", msgs)
	}
}

// TestAsyncExampleCanDisableWithEnv ensures WithEnv honours the opt-out flag.
func TestAsyncExampleCanDisableWithEnv(t *testing.T) {
	t.Setenv("SLOGCP_ASYNC", "false")

	var buf bytes.Buffer
	example, err := newAsyncExample(&buf)
	if err != nil {
		t.Fatalf("newAsyncExample: %v", err)
	}
	t.Cleanup(func() {
		if cerr := example.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	})

	example.logger.Info("sync-path")

	entries := decodeEntries(t, &buf)
	if len(entries) != 1 || entries[0]["message"] != "sync-path" {
		t.Fatalf("entries = %v, want message sync-path", entries)
	}

	if example.drops.count() != 0 {
		t.Fatalf("drops = %d, want 0 when async disabled", example.drops.count())
	}

	if _, ok := example.handler.Handler.(*slogcpasync.Handler); ok {
		t.Fatalf("expected inner handler to remain synchronous when disabled via env")
	}
}

// waitForDrops blocks until tracker records at least want drops or times out.
func waitForDrops(t *testing.T, tracker *dropTracker, want int) {
	t.Helper()

	deadline := time.After(time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()

	for {
		if tracker.count() >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d drops (saw %d)", want, tracker.count())
		case <-tick.C:
		}
	}
}

type blockingWriter struct {
	buf  bytes.Buffer
	gate chan struct{}
}

// newBlockingWriter returns a writer that blocks until release is called.
func newBlockingWriter() *blockingWriter {
	return &blockingWriter{
		gate: make(chan struct{}),
	}
}

// Write waits for the gate to open before buffering the data.
func (w *blockingWriter) Write(p []byte) (int, error) {
	<-w.gate
	return w.buf.Write(p)
}

// release opens the gate so future writes proceed.
func (w *blockingWriter) release() {
	select {
	case <-w.gate:
	default:
		close(w.gate)
	}
}

// entries decodes buffered log lines into JSON maps.
func (w *blockingWriter) entries(t *testing.T) []map[string]any {
	t.Helper()
	return decodeEntries(t, &w.buf)
}

// decodeEntries parses newline-delimited JSON from buf into map slices.
func decodeEntries(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	content := bytes.TrimSpace(buf.Bytes())
	if len(content) == 0 {
		return nil
	}

	lines := bytes.Split(content, []byte("\n"))
	out := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal log line: %v", err)
		}
		out = append(out, entry)
	}
	return out
}
