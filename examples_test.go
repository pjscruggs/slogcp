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

package slogcp_test

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

// TestExamplesExercisesEveryExampleModule by running go test ./... in each .examples module.
func TestExamplesExercisesEveryExampleModule(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	exampleDirs := discoverExampleModules(t, filepath.Join(root, ".examples"))

	for _, moduleDir := range exampleDirs {
		moduleDir := moduleDir
		relPath, err := filepath.Rel(root, moduleDir)
		if err != nil {
			relPath = moduleDir
		}
		relPath = filepath.ToSlash(relPath)

		t.Run(relPath, func(t *testing.T) {
			cmd := exec.Command("go", "test", "./...")
			cmd.Dir = moduleDir
			cmd.Env = os.Environ()

			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("go test ./... failed in %s: %v\n%s", relPath, err, out)
			}
		})
	}
}

// repoRoot returns the module root directory for integration-style tests.
func repoRoot(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() failed: %v", err)
	}
	return wd
}

// discoverExampleModules walks .examples and returns directories containing go.mod files.
func discoverExampleModules(t *testing.T, examplesRoot string) []string {
	t.Helper()

	info, err := os.Stat(examplesRoot)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip(".examples directory not present")
	}
	if err != nil {
		t.Fatalf("stat %s failed: %v", examplesRoot, err)
	}
	if !info.IsDir() {
		t.Fatalf(".examples path %s is not a directory", examplesRoot)
	}

	var modules []string
	err = filepath.WalkDir(examplesRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}

		goModPath := filepath.Join(path, "go.mod")
		if _, err := os.Stat(goModPath); err == nil {
			modules = append(modules, path)
			return filepath.SkipDir
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s failed: %v", examplesRoot, err)
	}

	if len(modules) == 0 {
		t.Skip("no example modules found under .examples")
	}

	sort.Strings(modules)
	return modules
}
