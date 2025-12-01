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
	"context"
	"log/slog"
	"testing"
)

// TestContextHelpersNilLogger ensures the contextual logging helpers safely
// handle a nil slog.Logger without panicking.
func TestContextHelpersNilLogger(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	helpers := map[string]func(context.Context, *slog.Logger, string, ...any){
		"DefaultContext":   DefaultContext,
		"DebugContext":     DebugContext,
		"InfoContext":      InfoContext,
		"WarnContext":      WarnContext,
		"ErrorContext":     ErrorContext,
		"NoticeContext":    NoticeContext,
		"CriticalContext":  CriticalContext,
		"AlertContext":     AlertContext,
		"EmergencyContext": EmergencyContext,
	}

	for name, fn := range helpers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fn(ctx, nil, "msg")
		})
	}
}
