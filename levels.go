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

	// LevelDebug maps to GCP DEBUG (100) severity, directly corresponding to
	// the standard slog.LevelDebug (-4). This level is used for detailed debugging
	// information that would be excessive at higher levels.
	LevelDebug Level = Level(slog.LevelDebug) // -4

	// LevelInfo maps to GCP INFO (200) severity, directly corresponding to
	// the standard slog.LevelInfo (0). This is the default level for routine
	// operational messages confirming normal operation.
	LevelInfo Level = Level(slog.LevelInfo) // 0

	// LevelNotice maps to GCP NOTICE (300) severity. This sits between Info and
	// Warn, used for significant but expected events worth highlighting.
	LevelNotice Level = 2

	// LevelWarn maps to GCP WARNING (400) severity, directly corresponding to
	// the standard slog.LevelWarn (4). Used for potentially harmful situations
	// or unexpected states that might indicate a problem.
	LevelWarn Level = Level(slog.LevelWarn) // 4

	// LevelError maps to GCP ERROR (500) severity, directly corresponding to
	// the standard slog.LevelError (8). Used for runtime errors that require
	// attention but don't necessarily impact overall application function.
	LevelError Level = Level(slog.LevelError) // 8

	// LevelCritical maps to GCP CRITICAL (600) severity. This is more severe
	// than Error, used for severe runtime errors that prevent some function.
	LevelCritical Level = 12

	// LevelAlert maps to GCP ALERT (700) severity. This indicates that action
	// must be taken immediately, such as when a component becomes unavailable.
	LevelAlert Level = 16

	// LevelEmergency maps to GCP EMERGENCY (800) severity. The highest severity
	// level, used when the system is unusable or in a catastrophic failure.
	LevelEmergency Level = 20

	// LevelDefault maps to GCP DEFAULT (0) severity. This is used for unspecified
	// or unknown severity and intentionally sorts above the other levels so that
	// default-severity entries are never filtered out by minimum level settings.
	LevelDefault Level = 30
)

type severityDescriptor struct {
	threshold Level
	levelName string
	fullName  string
	alias     string
}

var severityDescriptors = []severityDescriptor{
	{threshold: LevelDebug, levelName: "DEBUG", fullName: "DEBUG", alias: "D"},
	{threshold: LevelInfo, levelName: "INFO", fullName: "INFO", alias: "I"},
	{threshold: LevelNotice, levelName: "NOTICE", fullName: "NOTICE", alias: "N"},
	{threshold: LevelWarn, levelName: "WARN", fullName: "WARNING", alias: "W"},
	{threshold: LevelError, levelName: "ERROR", fullName: "ERROR", alias: "E"},
	{threshold: LevelCritical, levelName: "CRITICAL", fullName: "CRITICAL", alias: "C"},
	{threshold: LevelAlert, levelName: "ALERT", fullName: "ALERT", alias: "A"},
	{threshold: LevelEmergency, levelName: "EMERGENCY", fullName: "EMERGENCY", alias: "EMERG"},
	{threshold: LevelDefault, levelName: "DEFAULT", fullName: "DEFAULT", alias: "DEFAULT"},
}

// descriptorForLevel returns the severity descriptor whose threshold is at or
// below the provided level, falling back to the lowest descriptor if none
// match.
func descriptorForLevel(level Level) severityDescriptor {
	desc := severityDescriptors[0]
	for i := len(severityDescriptors) - 1; i >= 0; i-- {
		if level >= severityDescriptors[i].threshold {
			return severityDescriptors[i]
		}
	}
	return desc
}

// formatWithOffset renders base and, when offset is non-zero, appends the
// signed offset to capture intermediate severity values.
func formatWithOffset(base string, offset int) string {
	if offset == 0 {
		return base
	}
	return fmt.Sprintf("%s%+d", base, offset)
}

// String returns the canonical string representation of the Level, matching
// Google Cloud Logging severity names where applicable (e.g., "DEBUG", "NOTICE", "ERROR").
// For levels between defined constants, it returns the name of the nearest lower
// defined level plus the offset (e.g., "DEFAULT+1", "INFO+1", "NOTICE+1").
//
// This representation is used by the JSON handler for the "severity" field
// and provides a human-readable form of the level for debugging and display.
// Note that when logging to GCP, the String representation is not used directly;
// the numeric value is mapped to the appropriate GCP severity.
func (l Level) String() string {
	desc := descriptorForLevel(l)
	return formatWithOffset(desc.levelName, int(l-desc.threshold))
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

// severityString returns either the short Cloud Logging severity alias or the
// full severity name depending on useAliases.
func severityString(level slog.Level, useAliases bool) string {
	if useAliases {
		return severityAliasString(level)
	}
	return severityFullString(level)
}

// severityAliasString maps slog levels to Cloud Logging severity aliases.
// These shorter severity names are still recognized by Cloud Logging, and
// are converted to the full names in the Cloud Logging UI.
// Using these aliases saves about 1ns of JSON marshaling time per log entry.
func severityAliasString(level slog.Level) string {
	desc := descriptorForLevel(Level(level))
	return formatWithOffset(desc.alias, int(level-slog.Level(desc.threshold)))
}

// severityFullString maps slog levels to the full Cloud Logging severity names.
func severityFullString(level slog.Level) string {
	desc := descriptorForLevel(Level(level))
	return formatWithOffset(desc.fullName, int(level-slog.Level(desc.threshold)))
}
