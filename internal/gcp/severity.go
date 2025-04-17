package gcp

import (
	"log/slog"

	"cloud.google.com/go/logging"
)

// Internal constants mirroring `slogcp/levels.go` values, used for level
// comparisons during severity mapping to avoid import cycles.
const (
	internalLevelDefault   slog.Level = -8 // Corresponds to slogcp.LevelDefault
	internalLevelNotice    slog.Level = 2  // Corresponds to slogcp.LevelNotice
	internalLevelCritical  slog.Level = 12 // Corresponds to slogcp.LevelCritical
	internalLevelAlert     slog.Level = 16 // Corresponds to slogcp.LevelAlert
	internalLevelEmergency slog.Level = 20 // Corresponds to slogcp.LevelEmergency
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
