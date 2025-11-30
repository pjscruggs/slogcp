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

// Command phuslu-log demonstrates using slogcp with phuslu/log's
// AsyncWriter to deliver logs to a rotating file without blocking callers.
//
// This example is both documentation, and a test for `slogcp`.
// Our Github workflow tests if any changes to `slogcp` break the example.
package main

import (
	"log"
	"log/slog"
	"os"
	"path/filepath"

	phuslog "github.com/phuslu/log"
	"github.com/pjscruggs/slogcp"
)

func main() {
	logPath := filepath.Join("logs", "phuslu.log")
	asyncWriter := newAsyncFileWriter(logPath)

	handler, err := slogcp.NewHandler(os.Stdout, slogcp.WithRedirectWriter(asyncWriter))
	if err != nil {
		log.Fatalf("failed to create slogcp handler: %v", err)
	}
	defer handler.Close()
	defer asyncWriter.Close()

	slog.New(handler).Info("async slogcp logger ready",
		slog.String("destination", logPath),
		slog.String("writer", "phuslu/log AsyncWriter+FileWriter"),
	)
}

// newAsyncFileWriter builds a phuslu/log async writer that targets logPath.
func newAsyncFileWriter(logPath string) *phuslog.AsyncWriter {
	return &phuslog.AsyncWriter{
		ChannelSize: 1024,
		Writer: &phuslog.FileWriter{
			Filename:     logPath,
			MaxSize:      50 * 1024 * 1024,
			EnsureFolder: true,
			ProcessID:    true,
		},
	}
}
