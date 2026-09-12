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

package slogcp_test

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pjscruggs/slogcp/v2"
)

type consoleExporter struct{}

// Export consumes the borrowed entry before returning.
func (consoleExporter) Export(_ context.Context, entry slogcp.Entry) error {
	_, err := fmt.Printf("%s: %s\n", entry.Severity, entry.Payload["message"])
	return err
}

// ExampleNewHandlerWithExporter demonstrates an exporter outside the slogcp package.
func ExampleNewHandlerWithExporter() {
	handler, err := slogcp.NewHandlerWithExporter(consoleExporter{},
		slogcp.WithLevel(slog.LevelInfo),
	)
	if err != nil {
		panic(err)
	}
	logger := slog.New(handler)
	logger.InfoContext(context.Background(), "ready")
	if err := handler.Shutdown(context.Background()); err != nil {
		panic(err)
	}
	// Flush or close a buffering exporter here, after the handler has drained.
	// Output: INFO: ready
}
