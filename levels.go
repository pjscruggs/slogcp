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
	"fmt"
	"log/slog"
)

// Level represents the severity of a log event, extending slog.Level
// to include all Google Cloud Logging severity levels. It maintains the
// underlying integer representation compatible with slog.Level.
//
// Google Cloud Logging has 9 severity levels (DEFAULT, DEBUG, INFO, NOTICE,
// WARNING, ERROR, CRITICAL, ALERT, EMERGENCY) while standard slog has only 4
// (DEBUG, INFO, WARN, ERROR). This type bridges that gap by defining additional
// levels that map directly to GCP severities, allowing applications to use the
// full range of GCP logging levels while remaining compatible with slog.
//
// The integer values are chosen to maintain ordering and compatibility with
// the standard slog levels, allowing natural comparison operations.
type Level slog.Level

// Constants for GCP severity levels, mapped onto slog.Level integer values.
// The values are chosen to maintain order and provide some spacing,
// aligning with the slog philosophy while covering all GCP levels.
const (
	// LevelDefault maps to GCP DEFAULT (0) severity. This is used for unspecified
	// or unknown severity, and sorts below Debug in the severity hierarchy.
	// In GCP, this appears with a gray "—" (em dash) in the console.
	LevelDefault Level = -8

	// LevelDebug maps to GCP DEBUG (100) severity, directly corresponding to
	// the standard slog.LevelDebug (-4). This level is used for detailed debugging
	// information that would be excessive at higher levels.
	// In GCP, this appears with a gray "D" in the console.
	LevelDebug Level = Level(slog.LevelDebug) // -4

	// LevelInfo maps to GCP INFO (200) severity, directly corresponding to
	// the standard slog.LevelInfo (0). This is the default level for routine
	// operational messages confirming normal operation.
	// In GCP, this appears with a blue "I" in the console.
	LevelInfo Level = Level(slog.LevelInfo) // 0

	// LevelNotice maps to GCP NOTICE (300) severity. This sits between Info and
	// Warn, used for significant but expected events worth highlighting.
	// In GCP, this appears with a blue "N" in the console.
	LevelNotice Level = 2

	// LevelWarn maps to GCP WARNING (400) severity, directly corresponding to
	// the standard slog.LevelWarn (4). Used for potentially harmful situations
	// or unexpected states that might indicate a problem.
	// In GCP, this appears with a yellow "W" in the console.
	LevelWarn Level = Level(slog.LevelWarn) // 4

	// LevelError maps to GCP ERROR (500) severity, directly corresponding to
	// the standard slog.LevelError (8). Used for runtime errors that require
	// attention but don't necessarily impact overall application function.
	// In GCP, this appears with a red "E" in the console.
	LevelError Level = Level(slog.LevelError) // 8

	// LevelCritical maps to GCP CRITICAL (600) severity. This is more severe
	// than Error, used for severe runtime errors that prevent some function.
	// In GCP, this appears with a red "C" in the console.
	LevelCritical Level = 12

	// LevelAlert maps to GCP ALERT (700) severity. This indicates that action
	// must be taken immediately, such as when a component becomes unavailable.
	// In GCP, this appears with a red "A" in the console.
	LevelAlert Level = 16

	// LevelEmergency maps to GCP EMERGENCY (800) severity. The highest severity
	// level, used when the system is unusable or in a catastrophic failure.
	// In GCP, this appears with a red "E!" in the console.
	LevelEmergency Level = 20
)

// String returns the canonical string representation of the Level, matching
// Google Cloud Logging severity names where applicable (e.g., "DEBUG", "NOTICE", "ERROR").
// For levels between defined constants, it returns the name of the nearest lower
// defined level plus the offset (e.g., "DEFAULT+1", "INFO+1", "NOTICE+1").
//
// This representation is used by the fallback JSON handler for the "severity" field
// and provides a human-readable form of the level for debugging and display.
// Note that when logging to GCP, the String representation is not used directly;
// the numeric value is mapped to the appropriate GCP severity.
func (l Level) String() string {
	// First check for exact matches with defined constants
	switch l {
	case LevelDefault:
		return "DEFAULT"
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelNotice:
		return "NOTICE"
	case LevelWarn:
		return "WARN" // Note: GCP uses WARNING, but slog uses WARN. Keep WARN for consistency.
	case LevelError:
		return "ERROR"
	case LevelCritical:
		return "CRITICAL"
	case LevelAlert:
		return "ALERT"
	case LevelEmergency:
		return "EMERGENCY"
	}

	// For intermediate values, find the nearest lower defined level
	var baseLevel Level
	var baseName string

	switch {
	case l < LevelDefault:
		// For levels below DEFAULT, fall back to standard slog behavior
		return slog.Level(l).String()
	case l < LevelDebug: // Between DEFAULT and DEBUG
		baseLevel = LevelDefault
		baseName = "DEFAULT"
	case l < LevelInfo: // Between DEBUG and INFO
		baseLevel = LevelDebug
		baseName = "DEBUG"
	case l < LevelNotice: // Between INFO and NOTICE
		baseLevel = LevelInfo
		baseName = "INFO"
	case l < LevelWarn: // Between NOTICE and WARN
		baseLevel = LevelNotice
		baseName = "NOTICE"
	case l < LevelError: // Between WARN and ERROR
		baseLevel = LevelWarn
		baseName = "WARN"
	case l < LevelCritical: // Between ERROR and CRITICAL
		baseLevel = LevelError
		baseName = "ERROR"
	case l < LevelAlert: // Between CRITICAL and ALERT
		baseLevel = LevelCritical
		baseName = "CRITICAL"
	case l < LevelEmergency: // Between ALERT and EMERGENCY
		baseLevel = LevelAlert
		baseName = "ALERT"
	default: // Above EMERGENCY
		baseLevel = LevelEmergency
		baseName = "EMERGENCY"
	}

	// Calculate the offset and format the string
	offset := int(l - baseLevel)
	return fmt.Sprintf("%s+%d", baseName, offset)
}

// Level returns the underlying slog.Level value. This method allows slogcp.Level
// to satisfy the slog.Leveler interface, enabling its use in places like
// slog.HandlerOptions.Level and the standard slog.Logger methods.
//
// The slog.Leveler interface requires a Level() method that returns slog.Level.
// By implementing this method, Level can be used anywhere a standard slog level
// is expected. This seamless integration means you can pass a slogcp.Level
// to functions expecting a slog.Leveler, such as:
//
//   - As the Level field in slog.HandlerOptions
//   - When creating a new slog.LevelVar via Set()
//   - In comparison operations with standard slog levels
func (l Level) Level() slog.Level {
	return slog.Level(l)
}
