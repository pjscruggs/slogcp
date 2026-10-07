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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRejectsNonHeaderCommandsAndFlags(t *testing.T) {
	for _, args := range [][]string{
		{"version"},
		{"dependency", "resolve"},
		{"header", "diff"},
		{"header", "check", "--publish"},
		{"header", "fix", "-c"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			if err := run(args); err == nil {
				t.Fatalf("run(%q) unexpectedly succeeded", args)
			}
		})
	}
}

func TestRunFixAndCheckUseConfiguredHeaderAndPreserveSource(t *testing.T) {
	workingDir := t.TempDir()
	config := `header:
  license:
    spdx-id: Apache-2.0
    copyright-owner: "Example Owner"
    first-publication-year: &fp_year "2025"
    copyright-year: "2025-2026"
    software-name: "example"
  paths:
    - '**/*.go'
  comment: on-failure
  license-location-threshold: 1000
`
	if err := os.WriteFile(filepath.Join(workingDir, ".licenserc.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	source := "package sample\n\nfunc Value() int { return 7 }\n"
	path := filepath.Join(workingDir, "sample.go")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	oldWorkingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workingDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWorkingDir); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	if err := run([]string{"header", "check", "-c", ".licenserc.yaml"}); err == nil {
		t.Fatal("check accepted a source file without a license header")
	}
	if err := run([]string{"header", "fix", "-c", ".licenserc.yaml"}); err != nil {
		t.Fatalf("fix: %v", err)
	}
	fixed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(fixed), "// Copyright 2025-2026 Example Owner\n") {
		t.Fatalf("configured owner/year were not applied: %q", fixed[:min(len(fixed), 80)])
	}
	if !strings.HasSuffix(string(fixed), source) {
		t.Fatal("fix changed source bytes after the inserted header")
	}
	if err := run([]string{"header", "check", "-c", ".licenserc.yaml"}); err != nil {
		t.Fatalf("check after fix: %v", err)
	}
}

func TestRunRejectsStaleHeaderAndPrependsConfiguredYear(t *testing.T) {
	workingDir := t.TempDir()
	config := `header:
  license:
    spdx-id: Apache-2.0
    copyright-owner: "Example Owner"
    first-publication-year: &fp_year "2025"
    copyright-year: "2025-2026"
    software-name: "example"
  paths:
    - '**/*.go'
  comment: on-failure
  license-location-threshold: 1000
`
	if err := os.WriteFile(filepath.Join(workingDir, ".licenserc.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	oldHeader := "// Copyright 2024 Former Owner\n//\n// Licensed under the Apache License, Version 2.0 (the \"License\");\n// you may not use this file except in compliance with the License.\n\n"
	path := filepath.Join(workingDir, "stale.go")
	if err := os.WriteFile(path, []byte(oldHeader+"package sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldWorkingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workingDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWorkingDir); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	if err := run([]string{"header", "check", "-c", ".licenserc.yaml"}); err == nil {
		t.Fatal("check accepted a stale year and incorrect copyright owner")
	}
	if err := run([]string{"header", "fix", "-c", ".licenserc.yaml"}); err != nil {
		t.Fatalf("fix: %v", err)
	}
	fixed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(fixed), "// Copyright 2025-2026 Example Owner\n") {
		t.Fatalf("fix did not apply configured year and owner: %q", fixed[:min(len(fixed), 80)])
	}
	if !strings.Contains(string(fixed), oldHeader) {
		t.Fatal("fix did not preserve original bytes after the inserted header")
	}
	if err := run([]string{"header", "check", "-c", ".licenserc.yaml"}); err != nil {
		t.Fatalf("check after fix: %v", err)
	}
}

func TestRunRejectsInvalidConfigurationBeforeChangingFiles(t *testing.T) {
	validHeader := `header:
  license:
    spdx-id: Apache-2.0
    copyright-owner: "Example Owner"
    first-publication-year: &fp_year "2025"
    copyright-year: "2025-2026"
  paths:
    - '**/*.go'
`
	for name, config := range map[string]string{
		"missing header":         "{}\n",
		"missing license":        "header:\n  paths: ['**/*.go']\n",
		"misspelled license":     "header:\n  licence:\n    spdx-id: Apache-2.0\n",
		"unknown license field":  "header:\n  license:\n    spdx-idd: Apache-2.0\n",
		"empty resolved license": "header:\n  license:\n    spdx-id: NOT-A-REAL-SPDX-ID\n",
		"unknown header field":   validHeader + "  pathz: ['**/*.go']\n",
		"malformed YAML":         "header: [\n",
	} {
		t.Run(name, func(t *testing.T) {
			workingDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(workingDir, ".licenserc.yaml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(workingDir, "sample.go")
			original := []byte("package sample\n\nfunc Value() int { return 7 }\n")
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			oldWorkingDir, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chdir(workingDir); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Chdir(oldWorkingDir); err != nil {
					t.Errorf("restore working directory: %v", err)
				}
			})

			if err := run([]string{"header", "fix", "-c", ".licenserc.yaml"}); err == nil {
				t.Fatal("fix accepted invalid configuration")
			}
			actual, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(actual) != string(original) {
				t.Fatalf("invalid configuration changed source bytes:\n%s", actual)
			}
		})
	}
}
