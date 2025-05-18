//go:build unit
// +build unit

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
	"log/slog"
	"testing"

	"github.com/pjscruggs/slogcp"
)

// TestLevel_String verifies the string representation and underlying slog.Level value
// for all defined slogcp levels and intermediate values.
func TestLevel_String(t *testing.T) {
	testCases := []struct {
		level slogcp.Level
		want  string
		name  string // Descriptive name for t.Run subtest
	}{
		// Test cases cover exact matches for defined constants
		{slogcp.LevelDefault, "DEFAULT", "LevelDefault"},
		{slogcp.LevelDebug, "DEBUG", "LevelDebug"},
		{slogcp.LevelInfo, "INFO", "LevelInfo"},
		{slogcp.LevelNotice, "NOTICE", "LevelNotice"},
		{slogcp.LevelWarn, "WARN", "LevelWarn"},
		{slogcp.LevelError, "ERROR", "LevelError"},
		{slogcp.LevelCritical, "CRITICAL", "LevelCritical"},
		{slogcp.LevelAlert, "ALERT", "LevelAlert"},
		{slogcp.LevelEmergency, "EMERGENCY", "LevelEmergency"},
		// Test cases cover intermediate values between constants, checking "+N" logic
		{slogcp.LevelDefault + 1, "DEFAULT+1", "DefaultPlus1"},    // -7
		{slogcp.LevelDebug - 1, "DEFAULT+3", "BelowDebug"},        // -5
		{slogcp.LevelDebug + 1, "DEBUG+1", "DebugPlus1"},          // -3
		{slogcp.LevelInfo - 1, "DEBUG+3", "BelowInfo"},            // -1
		{slogcp.LevelInfo + 1, "INFO+1", "InfoPlus1"},             // 1
		{slogcp.LevelNotice - 1, "INFO+1", "BelowNotice"},         // 1
		{slogcp.LevelNotice + 1, "NOTICE+1", "NoticePlus1"},       // 3
		{slogcp.LevelWarn - 1, "NOTICE+1", "BelowWarn"},           // 3
		{slogcp.LevelWarn + 1, "WARN+1", "WarnPlus1"},             // 5
		{slogcp.LevelError - 1, "WARN+3", "BelowError"},           // 7
		{slogcp.LevelError + 1, "ERROR+1", "ErrorPlus1"},          // 9
		{slogcp.LevelCritical - 1, "ERROR+3", "BelowCritical"},    // 11
		{slogcp.LevelCritical + 1, "CRITICAL+1", "CriticalPlus1"}, // 13
		{slogcp.LevelAlert - 1, "CRITICAL+3", "BelowAlert"},       // 15
		{slogcp.LevelAlert + 1, "ALERT+1", "AlertPlus1"},          // 17
		{slogcp.LevelEmergency - 1, "ALERT+3", "BelowEmergency"},  // 19
		// Test cases cover edge cases below LevelDefault, verifying delegation to slog.Level.String()
		{slogcp.LevelDefault - 1, slog.Level(slogcp.LevelDefault - 1).String(), "BelowDefaultDelegation"},    // -9
		{slogcp.LevelDefault - 5, slog.Level(slogcp.LevelDefault - 5).String(), "FarBelowDefaultDelegation"}, // -13
		// Test cases cover edge cases far above LevelEmergency
		{slogcp.LevelEmergency + 1, "EMERGENCY+1", "EmergencyPlus1"},        // 21
		{slogcp.LevelEmergency + 100, "EMERGENCY+100", "FarAboveEmergency"}, // 120
	}

	for _, tc := range testCases {
		tc := tc // Capture range variable.
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel() // Subtests can run in parallel.
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
		if slogcp.LevelDebug.Level() != slog.LevelDebug {
			t.Errorf("LevelDebug (%v) does not match slog.LevelDebug (%v)", slogcp.LevelDebug.Level(), slog.LevelDebug)
		}
		if slogcp.LevelInfo.Level() != slog.LevelInfo {
			t.Errorf("LevelInfo (%v) does not match slog.LevelInfo (%v)", slogcp.LevelInfo.Level(), slog.LevelInfo)
		}
		if slogcp.LevelWarn.Level() != slog.LevelWarn {
			t.Errorf("LevelWarn (%v) does not match slog.LevelWarn (%v)", slogcp.LevelWarn.Level(), slog.LevelWarn)
		}
		if slogcp.LevelError.Level() != slog.LevelError {
			t.Errorf("LevelError (%v) does not match slog.LevelError (%v)", slogcp.LevelError.Level(), slog.LevelError)
		}
	})
}
