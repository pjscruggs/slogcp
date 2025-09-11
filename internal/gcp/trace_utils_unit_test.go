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
	"testing"
)

func TestFormatTraceResource(t *testing.T) {
	got := FormatTraceResource("proj", "abc123")
	want := "projects/proj/traces/abc123"
	if got != want {
		t.Errorf("FormatTraceResource() = %q, want %q", got, want)
	}
}

func TestSpanIDHexToDecimal(t *testing.T) {
	tests := []struct {
		hex     string
		wantDec string
		ok      bool
	}{
		{hex: "0000000000000001", wantDec: "1", ok: true},
		{hex: "5fa1c6de0d1e3e11", wantDec: "6891007561858694673", ok: true},
		{hex: "zz", wantDec: "", ok: false},
	}
	for _, tc := range tests {
		got, ok := SpanIDHexToDecimal(tc.hex)
		if ok != tc.ok || got != tc.wantDec {
			t.Errorf("SpanIDHexToDecimal(%q) = (%q, %v), want (%q, %v)", tc.hex, got, ok, tc.wantDec, tc.ok)
		}
	}
}

func TestBuildXCloudTraceContext(t *testing.T) {
	tests := []struct {
		traceID string
		spanHex string
		sampled bool
		want    string
	}{
		{"70f5c2c7b3c0d8eead4837399ac5b327", "5fa1c6de0d1e3e11", true, "70f5c2c7b3c0d8eead4837399ac5b327/6891007561858694673;o=1"},
		{"70f5c2c7b3c0d8eead4837399ac5b327", "", false, "70f5c2c7b3c0d8eead4837399ac5b327;o=0"},
		{"70f5c2c7b3c0d8eead4837399ac5b327", "zz", true, "70f5c2c7b3c0d8eead4837399ac5b327;o=1"},
	}
	for _, tc := range tests {
		got := BuildXCloudTraceContext(tc.traceID, tc.spanHex, tc.sampled)
		if got != tc.want {
			t.Errorf("BuildXCloudTraceContext(%q,%q,%v) = %q, want %q", tc.traceID, tc.spanHex, tc.sampled, got, tc.want)
		}
	}
}
