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

	"cloud.google.com/go/logging"
)

// Internal constants defining additional log severity levels beyond standard slog levels.
// These extend the slog.Level system to fully support Google Cloud Logging's severity scale.
const (
	internalLevelDefault   slog.Level = -8 // DEFAULT severity, lower than Debug
	internalLevelNotice    slog.Level = 2  // NOTICE severity, between Info and Warn
	internalLevelCritical  slog.Level = 12 // CRITICAL severity, above Error
	internalLevelAlert     slog.Level = 16 // ALERT severity, above Critical
	internalLevelEmergency slog.Level = 20 // EMERGENCY severity, highest level
)

// mapSlogLevelToGcpSeverity converts an slog.Level to the corresponding
// Google Cloud Logging Severity constant.
//
// It handles standard slog levels (Debug, Info, Warn, Error) and the
// extended levels defined internally (mirroring those in the main slogcp package)
// to provide a complete mapping to GCP's severity scale.
func mapSlogLevelToGcpSeverity(level slog.Level) logging.Severity {
	// Use comparisons based on the integer level values.
	// The order ensures correct mapping across the defined ranges.
	switch {
	case level <= internalLevelDefault: // Handles LevelDefault and any lower custom levels.
		return logging.Default
	case level <= slog.LevelDebug: // Handles levels > Default up to Debug.
		return logging.Debug
	case level <= slog.LevelInfo: // Handles levels > Debug up to Info.
		return logging.Info
	case level <= internalLevelNotice: // Handles levels > Info up to Notice.
		return logging.Notice
	case level <= slog.LevelWarn: // Handles levels > Notice up to Warn.
		return logging.Warning
	case level <= slog.LevelError: // Handles levels > Warn up to Error.
		return logging.Error
	case level <= internalLevelCritical: // Handles levels > Error up to Critical.
		return logging.Critical
	case level <= internalLevelAlert: // Handles levels > Critical up to Alert.
		return logging.Alert
	default: // level > internalLevelAlert (covers Emergency and any higher custom levels).
		return logging.Emergency
	}
}
