package gcp

import (
	"log/slog"
	"testing"

	"cloud.google.com/go/logging"
)

// TestMapSlogLevelToGcpSeverity verifies the mapping from slog.Level values
// (including slogcp extensions via internal constants) to the correct
// Google Cloud Logging Severity constants, focusing on boundary conditions.
func TestMapSlogLevelToGcpSeverity(t *testing.T) {
	testCases := []struct {
		level slog.Level
		want  logging.Severity
		name  string
	}{
		// Boundary: <= LevelDefault -> Default
		{internalLevelDefault, logging.Default, "Exact LevelDefault"},
		{internalLevelDefault - 1, logging.Default, "Below LevelDefault"},
		{slog.Level(-100), logging.Default, "Very Low Level"},

		// Boundary: LevelDefault < level <= LevelDebug -> Debug
		{internalLevelDefault + 1, logging.Debug, "Above Default"}, // -7
		{slog.LevelDebug - 1, logging.Debug, "Below Debug"},        // -5
		{slog.LevelDebug, logging.Debug, "Exact LevelDebug"},       // -4

		// Boundary: LevelDebug < level <= LevelInfo -> Info
		{slog.LevelDebug + 1, logging.Info, "Above Debug"}, // -3
		{slog.LevelInfo - 1, logging.Info, "Below Info"},   // -1
		{slog.LevelInfo, logging.Info, "Exact LevelInfo"},  // 0

		// Boundary: LevelInfo < level <= LevelNotice -> Notice
		{slog.LevelInfo + 1, logging.Notice, "Above Info"},         // 1
		{internalLevelNotice - 1, logging.Notice, "Below Notice"},  // 1
		{internalLevelNotice, logging.Notice, "Exact LevelNotice"}, // 2

		// Boundary: LevelNotice < level <= LevelWarn -> Warning
		{internalLevelNotice + 1, logging.Warning, "Above Notice"}, // 3
		{slog.LevelWarn - 1, logging.Warning, "Below Warn"},        // 3
		{slog.LevelWarn, logging.Warning, "Exact LevelWarn"},       // 4

		// Boundary: LevelWarn < level <= LevelError -> Error
		{slog.LevelWarn + 1, logging.Error, "Above Warn"},    // 5
		{slog.LevelError - 1, logging.Error, "Below Error"},  // 7
		{slog.LevelError, logging.Error, "Exact LevelError"}, // 8

		// Boundary: LevelError < level <= LevelCritical -> Critical
		{slog.LevelError + 1, logging.Critical, "Above Error"},           // 9
		{internalLevelCritical - 1, logging.Critical, "Below Critical"},  // 11
		{internalLevelCritical, logging.Critical, "Exact LevelCritical"}, // 12

		// Boundary: LevelCritical < level <= LevelAlert -> Alert
		{internalLevelCritical + 1, logging.Alert, "Above Critical"}, // 13
		{internalLevelAlert - 1, logging.Alert, "Below Alert"},       // 15
		{internalLevelAlert, logging.Alert, "Exact LevelAlert"},      // 16

		// Boundary: LevelAlert < level -> Emergency
		{internalLevelAlert + 1, logging.Emergency, "Above Alert"},          // 17
		{internalLevelEmergency - 1, logging.Emergency, "Below Emergency"},  // 19
		{internalLevelEmergency, logging.Emergency, "Exact LevelEmergency"}, // 20
		{internalLevelEmergency + 1, logging.Emergency, "Above Emergency"},  // 21
		{slog.Level(100), logging.Emergency, "Very High Level"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapSlogLevelToGcpSeverity(tc.level)
			if got != tc.want {
				// Use descriptive error message including level value and severity strings.
				t.Errorf("mapSlogLevelToGcpSeverity(level %d) = %v (%q), want %v (%q)",
					tc.level, got, got.String(), tc.want, tc.want.String())
			}
		})
	}
}
