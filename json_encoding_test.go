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
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type legacyJSONAttribute struct{}

// MarshalJSON exposes the legacy custom marshaler path.
func (legacyJSONAttribute) MarshalJSON() ([]byte, error) {
	return []byte(`{"kind":"legacy","html":"<tag>","line":"\u2028"}`), nil
}

type nativeJSONAttribute struct{}

// MarshalJSONTo exposes the native custom marshaler path, including pointer receivers.
func (*nativeJSONAttribute) MarshalJSONTo(encoder *jsontext.Encoder) error {
	return jsonv2.MarshalEncode(encoder, map[string]any{"kind": "native", "html": "<tag>", "line": "\u2028"})
}

// TestJSONEncodingCompatibility protects attribute representation independently
// of object member order, while checking newline framing and escaping explicitly.
func TestJSONEncodingCompatibility(t *testing.T) {
	type taggedPayload struct {
		Empty    string        `json:"empty,omitempty"`
		Zero     int           `json:"zero,omitempty"`
		Count    int           `json:"count,string"`
		Elapsed  time.Duration `json:"elapsed"`
		NilSlice []string      `json:"slice"`
	}

	cases := []struct {
		name    string
		payload map[string]any
	}{
		{"nil collections", map[string]any{"map": map[string]int(nil), "slice": []string(nil), "bytes": []byte(nil)}},
		{"invalid UTF8 keys and values", map[string]any{"key\xff": "value\xfe", "nested": map[string]any{"child\xff": "text\xfe"}}},
		{"HTML and JavaScript", map[string]any{"text": "<tag>&\u2028\u2029\n"}},
		{"raw JSON", map[string]any{"raw": json.RawMessage(` { "duplicate": 1, "duplicate": 2, "html": "<tag>" } `)}},
		{"typed tags durations and bytes", map[string]any{"typed": taggedPayload{Count: 42, Elapsed: time.Second}, "bytes": [2]byte{1, 2}, "duration": time.Millisecond}},
		{"custom marshalers", map[string]any{"legacy": legacyJSONAttribute{}, "native": &nativeJSONAttribute{}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var expected bytes.Buffer
			encoder := json.NewEncoder(&expected)
			encoder.SetEscapeHTML(false)
			if err := encoder.Encode(test.payload); err != nil {
				t.Fatalf("compatibility encoder: %v", err)
			}
			var actual bytes.Buffer
			handler := &jsonHandler{
				mu: &sync.Mutex{}, writer: &actual, internalLogger: slog.New(slog.DiscardHandler),
				bufferPool: &jsonBufferPool,
			}
			if err := handler.writeJSONPayload(test.payload); err != nil {
				t.Fatalf("writeJSONPayload: %v", err)
			}
			if bytes.Count(actual.Bytes(), []byte{'\n'}) != 1 || !bytes.HasSuffix(actual.Bytes(), []byte{'\n'}) {
				t.Fatalf("record must contain exactly one trailing newline: %q", actual.String())
			}
			var got, want any
			if err := json.Unmarshal(actual.Bytes(), &got); err != nil {
				t.Fatalf("decode actual: %v", err)
			}
			if err := json.Unmarshal(expected.Bytes(), &want); err != nil {
				t.Fatalf("decode expected: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("JSON values differ: got %s; want %s", actual.String(), expected.String())
			}
			if test.name == "HTML and JavaScript" {
				if !strings.Contains(actual.String(), "<tag>&") ||
					!strings.Contains(actual.String(), `\u2028\u2029\n`) {
					t.Fatalf("HTML must stay literal while JavaScript separators and newlines are escaped: %q", actual.String())
				}
			}
		})
	}
}

type partialJSONAttribute struct {
	calls  int
	failAt map[int]bool
}

// MarshalJSONTo writes enough content to flush a partial value before selected failures.
func (value *partialJSONAttribute) MarshalJSONTo(encoder *jsontext.Encoder) error {
	value.calls++
	for _, token := range []jsontext.Token{
		jsontext.BeginObject, jsontext.String("large"), jsontext.String(strings.Repeat("x", 128*1024)),
	} {
		if err := encoder.WriteToken(token); err != nil {
			return err
		}
	}
	if value.failAt[value.calls] {
		return errors.New("partial JSON attribute")
	}
	return encoder.WriteToken(jsontext.EndObject)
}

type jsonRecordWriter struct {
	buffer bytes.Buffer
	writes int
}

// Write records how many whole-record writes reach the final destination.
func (writer *jsonRecordWriter) Write(data []byte) (int, error) {
	writer.writes++
	return writer.buffer.Write(data)
}

// TestJSONEncodingPartialFailureIsStaged verifies failure, retry, and buffer reuse
// never expose partial JSON to the final writer.
func TestJSONEncodingPartialFailureIsStaged(t *testing.T) {
	var partial bytes.Buffer
	probe := &partialJSONAttribute{failAt: map[int]bool{1: true}}
	if err := encodeJSONLine(&partial, map[string]any{"stream": probe}); err == nil {
		t.Fatal("expected partial encoding failure")
	}
	if partial.Len() == 0 {
		t.Fatal("test must exercise a streaming encode that has already staged bytes")
	}
	for _, test := range []struct {
		name   string
		failAt map[int]bool
		failed bool
	}{
		{"sanitizer replaces invalid field", map[int]bool{1: true, 2: true}, false},
		{"transient failure retries", map[int]bool{1: true}, false},
		{"retry failure writes nothing", map[int]bool{1: true, 3: true}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output jsonRecordWriter
			handler := &jsonHandler{
				mu: &sync.Mutex{}, writer: &output, internalLogger: slog.New(slog.DiscardHandler),
				bufferPool: &jsonBufferPool,
			}
			attribute := &partialJSONAttribute{failAt: test.failAt}
			err := handler.writeJSONPayload(map[string]any{"message": "first", "stream": attribute})
			if (err != nil) != test.failed {
				t.Fatalf("writeJSONPayload error = %v, want failure %v", err, test.failed)
			}
			wantWrites := 1
			if test.failed {
				wantWrites = 0
				if output.buffer.Len() != 0 {
					t.Fatal("failed record reached the final writer")
				}
			}
			if output.writes != wantWrites {
				t.Fatalf("final writer calls = %d, want %d", output.writes, wantWrites)
			}
			if err := handler.writeJSONPayload(map[string]any{"message": "second"}); err != nil {
				t.Fatalf("following record failed: %v", err)
			}
			lines := bytes.Split(bytes.TrimSuffix(output.buffer.Bytes(), []byte{'\n'}), []byte{'\n'})
			if len(lines) != wantWrites+1 || output.writes != wantWrites+1 {
				t.Fatalf("expected one complete line and writer call per successful record; got %d lines, %d writes", len(lines), output.writes)
			}
			for _, line := range lines {
				if !json.Valid(line) {
					t.Fatalf("partial or malformed record escaped staging: %.100s", line)
				}
			}
			if string(lines[len(lines)-1]) != `{"message":"second"}` {
				t.Fatalf("following record contains stale buffer data: %s", lines[len(lines)-1])
			}
		})
	}
}
