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

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// TestLoggingModes exercises concurrent output and compares application fields
// after accounting for the Google stdout client's documented message envelope.
func TestLoggingModes(t *testing.T) {
	t.Setenv("CLOUD_RUN_JOB", "example-job")
	t.Setenv("CLOUD_RUN_EXECUTION", "example-execution")
	t.Setenv("CLOUD_RUN_TASK_INDEX", "0")
	t.Setenv("CLOUD_RUN_TASK_ATTEMPT", "0")
	t.Setenv("CLOUD_RUN_REGION", "example-region")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "benchmark-local")
	for _, payload := range []string{"small", "nested"} {
		t.Run(payload, func(t *testing.T) {
			var wantChecksum uint64
			var wantEntries map[int]map[string]any
			for _, mode := range []string{"none", "slogcp", "google-stdout"} {
				cfg := testConfig(mode, payload)
				var output bytes.Buffer
				res, err := runTrial(context.Background(), cfg, &output)
				if err != nil {
					t.Fatalf("%s: %v", mode, err)
				}
				if len(res.Errors) != 0 || res.CompletedElapsedNS < res.ProducerElapsedNS || res.CompletedElapsedNS <= 0 {
					t.Fatalf("%s: invalid result: %+v", mode, res)
				}
				if mode == "none" {
					wantChecksum = res.Checksum
					if output.Len() != 0 || res.OutputWrites != 0 {
						t.Fatal("no-logging mode wrote workload output")
					}
					continue
				}
				if res.Checksum != wantChecksum {
					t.Fatalf("%s: application work differs from no-logging control", mode)
				}
				if res.OutputWrites != int64(cfg.Count) {
					t.Fatalf("%s: measured writes = %d, want %d", mode, res.OutputWrites, cfg.Count)
				}
				entries := measuredEntries(t, cfg, output.Bytes())
				if mode == "slogcp" {
					wantEntries = entries
				} else if !reflect.DeepEqual(entries, wantEntries) {
					t.Fatalf("%s: application payloads differ\ngot: %#v\nwant: %#v", mode, entries, wantEntries)
				}
			}
		})
	}
}

func testConfig(mode, payload string) config {
	return config{
		Mode: mode, Payload: payload, Count: 4096, Concurrency: 4, Warmup: 2,
		RunID: "test-run", TrialID: "test-trial", Sink: "stdout", Project: "benchmark-local",
	}
}

func measuredEntries(t *testing.T, cfg config, data []byte) map[int]map[string]any {
	t.Helper()
	entries := make(map[int]map[string]any)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var wire map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &wire); err != nil {
			t.Fatalf("invalid structured output: %v", err)
		}
		entry := wire
		if cfg.Mode == "google-stdout" {
			var ok bool
			entry, ok = wire["message"].(map[string]any)
			if !ok {
				t.Fatalf("Google stdout payload is not an object: %v", wire)
			}
		}
		if entry["trial_id"] != cfg.TrialID {
			continue // Unmeasured warmup and Google instrumentation entries.
		}
		sequence := int(entry["sequence"].(float64))
		if _, exists := entries[sequence]; exists {
			t.Fatalf("duplicate sequence %d", sequence)
		}
		if wire["severity"] != "INFO" || entry["message"] != eventMessage || entry["run_id"] != cfg.RunID {
			t.Fatalf("incorrect event semantics: %v", wire)
		}
		if wire["logging.googleapis.com/insertId"] != cfg.RunID+"/"+cfg.TrialID+"/"+strconv.Itoa(sequence) {
			t.Fatal("incorrect unique insert ID")
		}
		labels, ok := wire["logging.googleapis.com/labels"].(map[string]any)
		if !ok || labels["run.googleapis.com/execution_name"] != "example-execution" {
			t.Fatalf("missing execution metadata: %v", wire)
		}
		delete(entry, "severity")
		delete(entry, "time")
		delete(entry, "logging.googleapis.com/insertId")
		delete(entry, "logging.googleapis.com/labels")
		entries[sequence] = entry
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(entries) != cfg.Count {
		t.Fatalf("received %d measured entries, want %d", len(entries), cfg.Count)
	}
	for sequence := range cfg.Count {
		if _, exists := entries[sequence]; !exists {
			t.Fatalf("missing sequence %d", sequence)
		}
	}
	return entries
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("sink unavailable") }

// TestOutputErrors rejects lost logs even though slog.Logger itself returns no error.
func TestOutputErrors(t *testing.T) {
	for _, mode := range []string{"slogcp", "google-stdout"} {
		t.Run(mode, func(t *testing.T) {
			res, err := runTrial(context.Background(), testConfig(mode, "small"), failingWriter{})
			if err == nil || len(res.Errors) == 0 {
				t.Fatalf("logging failure was not surfaced: %+v, %v", res, err)
			}
		})
	}
}

// TestResultFile ensures metrics remain separate from workload stdout.
func TestResultFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	err := run([]string{
		"-mode", "none", "-count", "5000", "-warmup", "1",
		"-run-id", "test-run", "-trial-id", "test-trial", "-result-file", path,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got result
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Config.Count != 5000 || got.Runtime.GoVersion == "" || len(got.Errors) != 0 {
		t.Fatalf("incomplete result: %+v", got)
	}
}

func TestRejectInvalidConfig(t *testing.T) {
	base := []string{"-run-id", "test", "-trial-id", "test", "-result-file", "unused.json"}
	for _, args := range [][]string{
		{"-mode", "unknown"}, {"-payload", "unknown"}, {"-count", "0"},
		{"-warmup", "0"}, {"-count", "1", "-concurrency", "2"},
		{"-mode", "google-api", "-sink", "discard"}, {"-sink", "unknown"},
	} {
		if _, err := parseConfig(append(append([]string{}, base...), args...)); err == nil {
			t.Errorf("accepted invalid configuration: %v", args)
		}
	}
}

func TestDiscardAccounting(t *testing.T) {
	res, err := runTrial(context.Background(), testConfig("slogcp", "small"), io.Discard)
	if err != nil || res.OutputWrites != 4096 || res.OutputBytes < 4096 {
		t.Fatalf("discard writer omitted encoding/accounting: %+v, %v", res, err)
	}
}

func TestCloudRunResource(t *testing.T) {
	t.Setenv("CLOUD_RUN_JOB", "example-job")
	t.Setenv("CLOUD_RUN_EXECUTION", "example-execution")
	t.Setenv("CLOUD_RUN_TASK_INDEX", "0")
	t.Setenv("CLOUD_RUN_TASK_ATTEMPT", "0")
	resource := monitoredResource(config{Project: "example-project", Location: "example-region"})
	if resource.Type != "cloud_run_job" || resource.Labels["job_name"] != "example-job" || resource.Labels["location"] != "example-region" {
		t.Fatalf("incorrect resource: %v", resource)
	}
	if executionLabels()["run.googleapis.com/execution_name"] != "example-execution" {
		t.Fatal("missing execution label")
	}
}

func TestQuantiles(t *testing.T) {
	samples := make([]int64, 100)
	for index := range samples {
		samples[index] = int64(100 - index)
	}
	if got := summarize(samples); got != (quantiles{P50: 50, P95: 95, P99: 99, Max: 100}) {
		t.Fatalf("incorrect nearest-rank percentiles: %v", got)
	}
}

func TestShortWrite(t *testing.T) {
	errs := new(errorList)
	writer := countingWriter{writer: shortWriter{}, errors: errs}
	if _, err := writer.Write([]byte("event")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write was not surfaced: %v", err)
	}
	if !strings.Contains(strings.Join(errs.snapshot(), " "), io.ErrShortWrite.Error()) {
		t.Fatal("short write was not recorded")
	}
}

type shortWriter struct{}

func (shortWriter) Write([]byte) (int, error) { return 1, nil }
