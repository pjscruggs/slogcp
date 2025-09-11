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

import "testing"

func TestLoadConfigTraceProjectID(t *testing.T) {
	t.Setenv("SLOGCP_LOG_TARGET", "stdout")
	t.Setenv("SLOGCP_PROJECT_ID", "base")
	t.Setenv("SLOGCP_TRACE_PROJECT_ID", "trace")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() returned %v, want nil", err)
	}
	if cfg.ProjectID != "base" {
		t.Errorf("ProjectID = %q, want %q", cfg.ProjectID, "base")
	}
	if cfg.TraceProjectID != "trace" {
		t.Errorf("TraceProjectID = %q, want %q", cfg.TraceProjectID, "trace")
	}
}

func TestLoadConfigTraceProjectIDDefault(t *testing.T) {
	t.Setenv("SLOGCP_LOG_TARGET", "stdout")
	t.Setenv("SLOGCP_PROJECT_ID", "base")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() returned %v, want nil", err)
	}
	if cfg.TraceProjectID != "base" {
		t.Errorf("TraceProjectID = %q, want %q", cfg.TraceProjectID, "base")
	}
}
