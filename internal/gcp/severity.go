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
	"fmt"
	"log/slog"

	"cloud.google.com/go/logging"
)

// Internal constants defining additional log severity levels beyond standard slog levels,
// mirroring the values used in the public slogcp package's Level type.
const (
	internalLevelNotice    slog.Level = 2  // NOTICE severity
	internalLevelCritical  slog.Level = 12 // CRITICAL severity
	internalLevelAlert     slog.Level = 16 // ALERT severity
	internalLevelEmergency slog.Level = 20 // EMERGENCY severity
	internalLevelDefault   slog.Level = 30 // DEFAULT severity
)

// mapSlogLevelToGcpSeverity converts an slog.Level to the corresponding
// Google Cloud Logging Severity constant. It handles standard slog levels
// and the extended levels defined internally.
func mapSlogLevelToGcpSeverity(level slog.Level) logging.Severity {
	switch {
	case level < slog.LevelInfo: // Covers Debug (-4) up to Info
		return logging.Debug
	case level < internalLevelNotice: // Covers Info (0) up to Notice
		return logging.Info
	case level < slog.LevelWarn: // Covers Notice (2) up to Warn
		return logging.Notice
	case level < slog.LevelError: // Covers Warn (4) up to Error
		return logging.Warning
	case level < internalLevelCritical: // Covers Error (8) up to Critical
		return logging.Error
	case level < internalLevelAlert: // Covers Critical (12) up to Alert
		return logging.Critical
	case level < internalLevelEmergency: // Covers Alert (16) up to Emergency
		return logging.Alert
	case level == internalLevelDefault: // Catches exactly the Default level (30)
		return logging.Default
	default: // Catches Emergency (20) and any higher custom levels
		return logging.Emergency
	}
}

// levelToString converts an slog.Level to its canonical string representation,
// matching Google Cloud Logging severity names where applicable (e.g., "DEBUG", "NOTICE", "ERROR").
// For levels between defined constants, it returns the name of the nearest lower
// defined level plus the offset (e.g., "DEFAULT+1", "INFO+1", "NOTICE+1").
// This is used by the handler in RedirectAsJSON mode for the "severity" field.
func levelToString(level slog.Level) string {
	// Helper to format the string with an offset.
	formatWithOffset := func(baseName string, offset slog.Level) string {
		if offset == 0 {
			return baseName
		}
		return fmt.Sprintf("%s%+d", baseName, offset)
	}

	// Handle levels by finding the correct range.
	switch {
	case level < slog.LevelInfo:
		return formatWithOffset("DEBUG", level-slog.LevelDebug)
	case level < internalLevelNotice:
		return formatWithOffset("INFO", level-slog.LevelInfo)
	case level < slog.LevelWarn:
		return formatWithOffset("NOTICE", level-internalLevelNotice)
	case level < slog.LevelError:
		return formatWithOffset("WARNING", level-slog.LevelWarn)
	case level < internalLevelCritical:
		return formatWithOffset("ERROR", level-slog.LevelError)
	case level < internalLevelAlert:
		return formatWithOffset("CRITICAL", level-internalLevelCritical)
	case level < internalLevelEmergency:
		return formatWithOffset("ALERT", level-internalLevelAlert)
	case level < internalLevelDefault:
		return formatWithOffset("EMERGENCY", level-internalLevelEmergency)
	default: // level >= internalLevelDefault
		return formatWithOffset("DEFAULT", level-internalLevelDefault)
	}
}
