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

const (
	internalLevelNotice    slog.Level = 2
	internalLevelCritical  slog.Level = 12
	internalLevelAlert     slog.Level = 16
	internalLevelEmergency slog.Level = 20
	internalLevelDefault   slog.Level = 30
)

// levelToString converts an slog.Level to its canonical string representation.
func levelToString(level slog.Level) string {
	formatWithOffset := func(baseName string, offset slog.Level) string {
		if offset == 0 {
			return baseName
		}
		return fmt.Sprintf("%s%+d", baseName, offset)
	}

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
	default:
		return formatWithOffset("DEFAULT", level-internalLevelDefault)
	}
}
