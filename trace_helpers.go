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
	"os"
	"strings"
)

// TraceAttributes extracts Cloud Trace aware attributes from ctx. The returned
// slice can be supplied to logger.With to correlate logs with traces when
// emitting per-request loggers.
//
// When projectID is empty, the helper falls back to environment variables in
// the following order: SLOGCP_TRACE_PROJECT_ID, SLOGCP_PROJECT_ID, and
// GOOGLE_CLOUD_PROJECT.
func TraceAttributes(ctx context.Context, projectID string) ([]slog.Attr, bool) {
	if ctx == nil {
		return nil, false
	}

	if projectID == "" {
		projectID = strings.TrimSpace(os.Getenv("SLOGCP_TRACE_PROJECT_ID"))
	}
	if projectID == "" {
		projectID = strings.TrimSpace(os.Getenv("SLOGCP_PROJECT_ID"))
	}
	if projectID == "" {
		projectID = strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT"))
	}

	fmtTrace, rawTrace, rawSpan, sampled, sc := ExtractTraceSpan(ctx, projectID)
	if !sc.IsValid() {
		return nil, false
	}

	attrs := []slog.Attr{
		slog.String("trace_id", rawTrace),
		slog.String(SpanKey, rawSpan),
		slog.Bool(SampledKey, sampled),
	}
	if fmtTrace != "" {
		attrs = append(attrs, slog.String(TraceKey, fmtTrace))
	}
	return attrs, true
}
