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
	"log/slog"
	"testing"
)

// TestParseLevelEnv verifies parsing of log level strings with both custom and package defaults.
func TestParseLevelEnv(t *testing.T) {
	customDefault := slog.LevelWarn
	packageDefault := slogcpDefaultLogLevel

	testCases := []struct {
		name       string
		levelStr   string
		defaultVal slog.Level
		want       slog.Level
	}{
		{"Empty uses custom default", "", customDefault, customDefault},
		{"Empty uses package default", "", packageDefault, packageDefault},
		{"Whitespace uses custom default", "   ", customDefault, customDefault},
		{"Valid DEFAULT lowercase", "default", customDefault, internalLevelDefault},
		{"Valid DEFAULT uppercase", "DEFAULT", customDefault, internalLevelDefault},
		{"Valid DEBUG lowercase", "debug", customDefault, slog.LevelDebug},
		{"Valid DEBUG uppercase", "DEBUG", customDefault, slog.LevelDebug},
		{"Valid INFO mixed case", "Info", customDefault, slog.LevelInfo},
		{"Valid NOTICE lowercase", "notice", customDefault, internalLevelNotice},
		{"Valid WARN alias", "warning", customDefault, slog.LevelWarn},
		{"String with spaces", " info ", customDefault, slog.LevelInfo},
		{"Numeric 0", "0", customDefault, slog.Level(0)},
		{"Numeric -4", "-4", customDefault, slog.Level(-4)},
		{"Numeric 8", "8", customDefault, slog.Level(8)},
		{"Invalid string uses custom default", "verbose", customDefault, customDefault},
		{"Invalid string uses package default", "verbose", packageDefault, packageDefault},
		{"Invalid numeric uses custom default", "1.5", customDefault, customDefault},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLevelEnv(tc.levelStr, tc.defaultVal)
			if got != tc.want {
				t.Errorf("parseLevelEnv(%q, %v) = %v; want %v",
					tc.levelStr, tc.defaultVal, got, tc.want)
			}
		})
	}
}

// TestParseBoolEnv verifies parsing of boolean strings with both custom and package defaults.
func TestParseBoolEnv(t *testing.T) {
	customDefault := false
	packageDefault := slogcpDefaultAddSource

	testCases := []struct {
		name       string
		boolStr    string
		defaultVal bool
		want       bool
	}{
		{"Empty uses custom default", "", customDefault, customDefault},
		{"Empty uses package default", "", packageDefault, packageDefault},
		{"Whitespace uses custom default", "   ", customDefault, customDefault},
		{"Valid true lowercase", "true", customDefault, true},
		{"Valid true uppercase", "TRUE", customDefault, true},
		{"Valid false mixed", "False", customDefault, false},
		{"Valid 1", "1", customDefault, true},
		{"Valid 0", "0", customDefault, false},
		{"String with spaces", " true ", customDefault, true},
		{"Invalid string uses custom default", "yes", customDefault, customDefault},
		{"Invalid string uses package default", "enabled", packageDefault, packageDefault},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseBoolEnv(tc.boolStr, tc.defaultVal)
			if got != tc.want {
				t.Errorf("parseBoolEnv(%q, %v) = %v; want %v",
					tc.boolStr, tc.defaultVal, got, tc.want)
			}
		})
	}
}

// TestConfigReplaceAttrFunc checks that ReplaceAttrFunc handles []string correctly.
func TestConfigReplaceAttrFunc(t *testing.T) {
	// Track received group slices and attributes
	var seenGroups [][]string
	replacer := func(groups []string, attr slog.Attr) slog.Attr {
		// copy slice to avoid aliasing
		grp := append([]string(nil), groups...)
		seenGroups = append(seenGroups, grp)
		return attr
	}

	cfg := Config{ReplaceAttrFunc: replacer}
	if cfg.ReplaceAttrFunc == nil {
		t.Fatal("ReplaceAttrFunc should be set")
	}

	cases := [][]string{{}, {"a"}, {"x", "y"}}
	for idx, grp := range cases {
		_ = cfg.ReplaceAttrFunc(grp, slog.String("k", "v"))
		if len(seenGroups) <= idx {
			t.Errorf("Expected call for case %d, got none", idx)
			continue
		}
		got := seenGroups[idx]
		if len(got) != len(grp) {
			t.Errorf("Case %d: group length = %d; want %d", idx, len(got), len(grp))
		}
	}
}
