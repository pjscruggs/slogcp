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
	internalLevelDefault   slog.Level = -8 // DEFAULT severity
	internalLevelNotice    slog.Level = 2  // NOTICE severity
	internalLevelCritical  slog.Level = 12 // CRITICAL severity
	internalLevelAlert     slog.Level = 16 // ALERT severity
	internalLevelEmergency slog.Level = 20 // EMERGENCY severity
)

// mapSlogLevelToGcpSeverity converts an slog.Level to the corresponding
// Google Cloud Logging Severity constant. It handles standard slog levels
// and the extended levels defined internally.
func mapSlogLevelToGcpSeverity(level slog.Level) logging.Severity {
	switch {
	case level < slog.LevelDebug: // Covers Default (-8) and lower
		return logging.Default
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
	default: // Covers Emergency (20) and higher
		return logging.Emergency
	}
}

// levelToString converts an slog.Level to its canonical string representation,
// matching Google Cloud Logging severity names where applicable (e.g., "DEBUG", "NOTICE", "ERROR").
// For levels between defined constants, it returns the name of the nearest lower
// defined level plus the offset (e.g., "DEFAULT+1", "INFO+1", "NOTICE+1").
// This is used by the handler in RedirectAsJSON mode for the "severity" field.
func levelToString(level slog.Level) string {
	// Check for exact matches with defined constants (both slog standard and internal extended)
	switch level {
	case internalLevelDefault:
		return "DEFAULT"
	case slog.LevelDebug:
		return "DEBUG"
	case slog.LevelInfo:
		return "INFO"
	case internalLevelNotice:
		return "NOTICE"
	case slog.LevelWarn:
		// Note: GCP uses 'WARNING', but slog uses 'WARN'. Use GCP name for structured JSON output.
		return "WARNING"
	case slog.LevelError:
		return "ERROR"
	case internalLevelCritical:
		return "CRITICAL"
	case internalLevelAlert:
		return "ALERT"
	case internalLevelEmergency:
		return "EMERGENCY"
	}

	// For intermediate values, find the nearest lower defined level and name
	var baseLevel slog.Level
	var baseName string

	switch {
	case level < internalLevelDefault:
		// For levels below DEFAULT, use "DEFAULT-[offset]" for clarity
		offset := int(internalLevelDefault - level)
		return fmt.Sprintf("DEFAULT-%d", offset)
	case level < slog.LevelDebug: // Between DEFAULT and DEBUG
		baseLevel = internalLevelDefault
		baseName = "DEFAULT"
	case level < slog.LevelInfo: // Between DEBUG and INFO
		baseLevel = slog.LevelDebug
		baseName = "DEBUG"
	case level < internalLevelNotice: // Between INFO and NOTICE
		baseLevel = slog.LevelInfo
		baseName = "INFO"
	case level < slog.LevelWarn: // Between NOTICE and WARN
		baseLevel = internalLevelNotice
		baseName = "NOTICE"
	case level < slog.LevelError: // Between WARN and ERROR
		baseLevel = slog.LevelWarn
		baseName = "WARNING" // Use GCP name
	case level < internalLevelCritical: // Between ERROR and CRITICAL
		baseLevel = slog.LevelError
		baseName = "ERROR"
	case level < internalLevelAlert: // Between CRITICAL and ALERT
		baseLevel = internalLevelCritical
		baseName = "CRITICAL"
	case level < internalLevelEmergency: // Between ALERT and EMERGENCY
		baseLevel = internalLevelAlert
		baseName = "ALERT"
	default: // Above EMERGENCY
		baseLevel = internalLevelEmergency
		baseName = "EMERGENCY"
	}

	// Calculate the offset and format the string
	offset := int(level - baseLevel)
	return fmt.Sprintf("%s+%d", baseName, offset)
}
