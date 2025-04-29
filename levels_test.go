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

package slogcp

import (
	"log/slog"
	"testing"
)

// TestLevel_String verifies the string representation and underlying slog.Level value
// for all defined slogcp levels and intermediate values.
func TestLevel_String(t *testing.T) {
	testCases := []struct {
		level Level
		want  string
		name  string // Descriptive name for t.Run subtest
	}{
		// Test cases cover exact matches for defined constants
		{LevelDefault, "DEFAULT", "LevelDefault"},
		{LevelDebug, "DEBUG", "LevelDebug"},
		{LevelInfo, "INFO", "LevelInfo"},
		{LevelNotice, "NOTICE", "LevelNotice"},
		{LevelWarn, "WARN", "LevelWarn"},
		{LevelError, "ERROR", "LevelError"},
		{LevelCritical, "CRITICAL", "LevelCritical"},
		{LevelAlert, "ALERT", "LevelAlert"},
		{LevelEmergency, "EMERGENCY", "LevelEmergency"},

		// Test cases cover intermediate values between constants, checking "+N" logic
		{LevelDefault + 1, "DEFAULT+1", "DefaultPlus1"},
		{LevelDebug - 1, "DEFAULT+3", "BelowDebug"},           // -5
		{LevelDebug + 1, "DEBUG+1", "DebugPlus1"},             // -3
		{LevelInfo - 1, "DEBUG+3", "BelowInfo"},               // -1
		{LevelInfo + 1, "INFO+1", "InfoPlus1"},                // 1
		{LevelNotice - 1, "INFO+1", "BelowNotice"},            // 1
		{LevelNotice + 1, "NOTICE+1", "NoticePlus1"},          // 3
		{LevelWarn - 1, "NOTICE+1", "BelowWarn"},              // 3
		{LevelWarn + 1, "WARN+1", "WarnPlus1"},                // 5
		{LevelError - 1, "WARN+3", "BelowError"},              // 7
		{LevelError + 1, "ERROR+1", "ErrorPlus1"},             // 9
		{LevelCritical - 1, "ERROR+3", "BelowCritical"},       // 11
		{LevelCritical + 1, "CRITICAL+1", "CriticalPlus1"},    // 13
		{LevelAlert - 1, "CRITICAL+3", "BelowAlert"},          // 15
		{LevelAlert + 1, "ALERT+1", "AlertPlus1"},             // 17
		{LevelEmergency - 1, "ALERT+3", "BelowEmergency"},     // 19
		{LevelEmergency + 1, "EMERGENCY+1", "EmergencyPlus1"}, // 21

		// Test cases cover edge cases below LevelDefault, verifying delegation to slog.Level.String()
		{LevelDefault - 1, slog.Level(LevelDefault - 1).String(), "BelowDefaultDelegation"},    // -9
		{LevelDefault - 5, slog.Level(LevelDefault - 5).String(), "FarBelowDefaultDelegation"}, // -13

		// Test cases cover edge cases far above LevelEmergency
		{LevelEmergency + 100, "EMERGENCY+100", "FarAboveEmergency"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Verify the String() method output.
			gotString := tc.level.String()
			if gotString != tc.want {
				t.Errorf("Level(%d).String() = %q, want %q", tc.level, gotString, tc.want)
			}

			// Verify the Level() method returns the correct underlying slog.Level.
			gotLevel := tc.level.Level()
			wantLevel := slog.Level(tc.level)
			if gotLevel != wantLevel {
				t.Errorf("Level(%d).Level() = %v, want %v", tc.level, gotLevel, wantLevel)
			}
		})
	}

	// ConstantValueChecks ensures alignment with standard slog levels.
	t.Run("ConstantValueChecks", func(t *testing.T) {
		if LevelDebug.Level() != slog.LevelDebug {
			t.Errorf("LevelDebug (%v) does not match slog.LevelDebug (%v)", LevelDebug.Level(), slog.LevelDebug)
		}
		if LevelInfo.Level() != slog.LevelInfo {
			t.Errorf("LevelInfo (%v) does not match slog.LevelInfo (%v)", LevelInfo.Level(), slog.LevelInfo)
		}
		if LevelWarn.Level() != slog.LevelWarn {
			t.Errorf("LevelWarn (%v) does not match slog.LevelWarn (%v)", LevelWarn.Level(), slog.LevelWarn)
		}
		if LevelError.Level() != slog.LevelError {
			t.Errorf("LevelError (%v) does not match slog.LevelError (%v)", LevelError.Level(), slog.LevelError)
		}
	})
}
