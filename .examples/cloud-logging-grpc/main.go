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

// Command cloud-logging-grpc sends a structured log through the Cloud Logging API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"time"

	"cloud.google.com/go/logging"

	"github.com/pjscruggs/slogcp/v2"

	slogcpgrpc "github.com/pjscruggs/slogcp-grpc"
)

// main writes the example log and reports setup or delivery errors.
func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

// run reads the destination project and owns the logging client's lifetime.
func run(ctx context.Context) (result error) {
	projectID := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if projectID == "" {
		return errors.New("set GOOGLE_CLOUD_PROJECT to your logging project")
	}
	client, err := logging.NewClient(ctx, projectID)
	if err != nil {
		return fmt.Errorf("create logging client %w", err)
	}
	defer func() { result = errors.Join(result, client.Close()) }()
	client.OnError = func(err error) { fmt.Fprintln(os.Stderr, err) }
	return emit(ctx, client, projectID)
}

// emit configures buffered delivery and drains the handler and exporter.
func emit(ctx context.Context, client *logging.Client, projectID string) error {
	cloudLogger := client.Logger("application",
		logging.DelayThreshold(100*time.Millisecond),
		logging.EntryCountThreshold(1000),
		logging.ConcurrentWriteLimit(4),
		logging.BufferedByteLimit(32<<20),
		logging.ContextFunc(func() (context.Context, func()) {
			return context.WithTimeout(context.Background(), 10*time.Second)
		}),
	)
	exporter, err := slogcpgrpc.NewExporter(cloudLogger)
	if err != nil {
		return fmt.Errorf("create exporter %w", err)
	}
	handler, err := slogcp.NewHandlerWithExporter(exporter, slogcp.WithTraceProjectID(projectID))
	if err != nil {
		return fmt.Errorf("create handler %w", err)
	}
	logger := slog.New(handler)
	logger.InfoContext(ctx, "service ready", "transport", "grpc")
	return errors.Join(handler.Close(), exporter.Flush())
}
