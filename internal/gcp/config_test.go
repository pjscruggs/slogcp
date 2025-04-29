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

// TestParseLevelWithDefault verifies the logic for parsing level strings with fallback.
func TestParseLevelWithDefault(t *testing.T) {
	customDefaultLevel := slog.LevelWarn // Use a non-standard default for testing fallback

	testCases := []struct {
		name       string
		levelStr   string
		defaultLvl slog.Level
		envVarName string // For error message clarity
		want       slog.Level
	}{
		{"Empty string uses custom default", "", customDefaultLevel, "TEST_LEVEL", customDefaultLevel},
		{"Empty string uses package default", "", defaultLevel, "LOG_LEVEL", defaultLevel}, // Test package default
		{"Whitespace string uses default", "  ", customDefaultLevel, "TEST_LEVEL", customDefaultLevel},
		{"Valid DEFAULT lowercase", "default", customDefaultLevel, "TEST_LEVEL", internalLevelDefault},
		{"Valid DEFAULT uppercase", "DEFAULT", customDefaultLevel, "TEST_LEVEL", internalLevelDefault},
		{"Valid DEBUG lowercase", "debug", customDefaultLevel, "TEST_LEVEL", slog.LevelDebug},
		{"Valid DEBUG uppercase", "DEBUG", customDefaultLevel, "TEST_LEVEL", slog.LevelDebug},
		{"Valid INFO lowercase", "info", customDefaultLevel, "TEST_LEVEL", slog.LevelInfo},
		{"Valid INFO mixed case", "Info", customDefaultLevel, "TEST_LEVEL", slog.LevelInfo},
		{"Valid NOTICE lowercase", "notice", customDefaultLevel, "TEST_LEVEL", internalLevelNotice},
		{"Valid NOTICE uppercase", "NOTICE", customDefaultLevel, "TEST_LEVEL", internalLevelNotice},
		{"Valid WARN lowercase", "warn", customDefaultLevel, "TEST_LEVEL", slog.LevelWarn},
		{"Valid WARNING lowercase", "warning", customDefaultLevel, "TEST_LEVEL", slog.LevelWarn}, // Alias
		{"Valid WARN uppercase", "WARN", customDefaultLevel, "TEST_LEVEL", slog.LevelWarn},
		{"Valid ERROR lowercase", "error", customDefaultLevel, "TEST_LEVEL", slog.LevelError},
		{"Valid ERROR uppercase", "ERROR", customDefaultLevel, "TEST_LEVEL", slog.LevelError},
		{"Valid CRITICAL lowercase", "critical", customDefaultLevel, "TEST_LEVEL", internalLevelCritical},
		{"Valid CRITICAL uppercase", "CRITICAL", customDefaultLevel, "TEST_LEVEL", internalLevelCritical},
		{"Valid ALERT lowercase", "alert", customDefaultLevel, "TEST_LEVEL", internalLevelAlert},
		{"Valid ALERT uppercase", "ALERT", customDefaultLevel, "TEST_LEVEL", internalLevelAlert},
		{"Valid EMERGENCY lowercase", "emergency", customDefaultLevel, "TEST_LEVEL", internalLevelEmergency},
		{"Valid EMERGENCY uppercase", "EMERGENCY", customDefaultLevel, "TEST_LEVEL", internalLevelEmergency},
		{"Invalid string uses custom default", "verbose", customDefaultLevel, "TEST_LEVEL", customDefaultLevel},
		{"Invalid string uses package default", "verbose", defaultLevel, "LOG_LEVEL", defaultLevel}, // Test package default
		{"Partial match uses default", "inf", customDefaultLevel, "TEST_LEVEL", customDefaultLevel},
		{"String with spaces", " info ", customDefaultLevel, "TEST_LEVEL", slog.LevelInfo},
		{"Numerical Level 0", "0", customDefaultLevel, "TEST_LEVEL", slog.Level(0)},
		{"Numerical Level -4", "-4", customDefaultLevel, "TEST_LEVEL", slog.Level(-4)},
		{"Numerical Level 8", "8", customDefaultLevel, "TEST_LEVEL", slog.Level(8)},
		{"Numerical Level with spaces", " 12 ", customDefaultLevel, "TEST_LEVEL", slog.Level(12)},
		{"Invalid Numerical Level", "1.5", customDefaultLevel, "TEST_LEVEL", customDefaultLevel},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Note: This test does not capture stderr to verify warning logs on parse failure.
			got := parseLevelWithDefault(tc.levelStr, tc.defaultLvl, tc.envVarName)
			if got != tc.want {
				// Use descriptive error message.
				t.Errorf("parseLevelWithDefault(%q, %v, %q) = %v, want %v",
					tc.levelStr, tc.defaultLvl, tc.envVarName, got, tc.want)
			}
		})
	}
}

// TestParseLevel verifies the top-level helper using the package default level.
func TestParseLevel(t *testing.T) {
	testCases := []struct {
		name     string
		levelStr string
		want     slog.Level
	}{
		{"Empty uses default INFO", "", slog.LevelInfo},
		{"Valid DEBUG", "debug", slog.LevelDebug},
		{"Valid INFO", "info", slog.LevelInfo},
		{"Valid NOTICE", "notice", internalLevelNotice},
		{"Invalid uses default INFO", "invalid", slog.LevelInfo},
		{"Numerical Level -4", "-4", slog.Level(-4)},
		{"Numerical Level 8", "8", slog.Level(8)},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLevel(tc.levelStr)
			if got != tc.want {
				t.Errorf("parseLevel(%q) = %v, want %v", tc.levelStr, got, tc.want)
			}
		})
	}
}

// TestParseBool verifies the logic for parsing boolean strings with fallback.
func TestParseBool(t *testing.T) {
	customDefaultBool := false // Use false as default for testing

	testCases := []struct {
		name       string
		boolStr    string
		defaultVal bool
		envVarName string // For error message clarity
		want       bool
	}{
		{"Empty string uses custom default", "", customDefaultBool, "TEST_BOOL", customDefaultBool},
		{"Empty string uses package default", "", defaultAddSource, "LOG_SOURCE_LOCATION", defaultAddSource}, // Test package default
		{"Whitespace string uses default", "  ", customDefaultBool, "TEST_BOOL", customDefaultBool},
		{"Valid true lowercase", "true", customDefaultBool, "TEST_BOOL", true},
		{"Valid true uppercase", "TRUE", customDefaultBool, "TEST_BOOL", true},
		{"Valid true mixed case", "True", customDefaultBool, "TEST_BOOL", true},
		{"Valid false lowercase", "false", customDefaultBool, "TEST_BOOL", false},
		{"Valid false uppercase", "FALSE", customDefaultBool, "TEST_BOOL", false},
		{"Valid false mixed case", "False", customDefaultBool, "TEST_BOOL", false},
		{"Valid 1", "1", customDefaultBool, "TEST_BOOL", true},            // strconv.ParseBool handles "1"
		{"Valid 0", "0", customDefaultBool, "TEST_BOOL", false},           // strconv.ParseBool handles "0"
		{"Valid t lowercase", "t", customDefaultBool, "TEST_BOOL", true},  // strconv.ParseBool handles "t"
		{"Valid f lowercase", "f", customDefaultBool, "TEST_BOOL", false}, // strconv.ParseBool handles "f"
		{"Valid T uppercase", "T", customDefaultBool, "TEST_BOOL", true},  // strconv.ParseBool handles "T"
		{"Valid F uppercase", "F", customDefaultBool, "TEST_BOOL", false}, // strconv.ParseBool handles "F"
		{"Invalid string uses custom default", "yes", customDefaultBool, "TEST_BOOL", customDefaultBool},
		{"Invalid string uses package default", "enabled", defaultAddSource, "LOG_SOURCE_LOCATION", defaultAddSource}, // Test package default
		{"String with spaces", " true ", customDefaultBool, "TEST_BOOL", true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Note: This test does not capture stderr to verify warning logs on parse failure.
			got := parseBool(tc.boolStr, tc.defaultVal, tc.envVarName)
			if got != tc.want {
				// Use descriptive error message.
				t.Errorf("parseBool(%q, %v, %q) = %v, want %v",
					tc.boolStr, tc.defaultVal, tc.envVarName, got, tc.want)
			}
		})
	}
}
