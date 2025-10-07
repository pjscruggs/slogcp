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
	"log/slog"
	"testing"
	"time"

	logpb "google.golang.org/genproto/googleapis/logging/v2"
)

func TestExtractOperationFromRecord(t *testing.T) {
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	r.AddAttrs(slog.Group(operationGroupKey,
		slog.String("id", "123"),
		slog.String("producer", "prod"),
		slog.Bool("first", true),
		slog.Bool("last", false),
	))
	op := ExtractOperationFromRecord(r)
	if op == nil {
		t.Fatalf("ExtractOperationFromRecord() returned nil, want non-nil")
	}
	if op.Id != "123" || op.Producer != "prod" || !op.First || op.Last {
		t.Errorf("ExtractOperationFromRecord() = %#v", op)
	}

	r2 := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	if got := ExtractOperationFromRecord(r2); got != nil {
		t.Errorf("ExtractOperationFromRecord() = %#v, want nil", got)
	}
}

func TestExtractOperationFromPayload(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		want    *logpb.LogEntryOperation
	}{
		{
			name: "valid",
			payload: map[string]any{
				operationGroupKey: map[string]any{
					"id":       "123",
					"producer": "prod",
					"first":    true,
					"last":     "true",
				},
			},
			want: &logpb.LogEntryOperation{Id: "123", Producer: "prod", First: true, Last: true},
		},
		{
			name:    "missing",
			payload: map[string]any{},
			want:    nil,
		},
		{
			name:    "wrong type",
			payload: map[string]any{operationGroupKey: "oops"},
			want:    nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractOperationFromPayload(tc.payload)
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("ExtractOperationFromPayload() = %#v, want %#v", got, tc.want)
			}
			if got != nil {
				if got.Id != tc.want.Id || got.Producer != tc.want.Producer || got.First != tc.want.First || got.Last != tc.want.Last {
					t.Errorf("ExtractOperationFromPayload() = %#v, want %#v", got, tc.want)
				}
			}
		})
	}
}
