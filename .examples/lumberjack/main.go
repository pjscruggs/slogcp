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

// Command lumberjack demonstrates integrating slogcp with a rotating
// lumberjack writer.
//
// This example is both documentation, and a test for `slogcp`.
// Our Github workflow tests if any changes to `slogcp` break the example.
package main

import (
	"log"
	"log/slog"

	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/pjscruggs/slogcp"
)

func main() {
	rolling := &lumberjack.Logger{
		Filename:   "slogcp-rolling.log",
		MaxSize:    1,
		MaxBackups: 3,
		MaxAge:     7,
		Compress:   true,
	}
	defer func() {
		if err := rolling.Close(); err != nil {
			log.Printf("close lumberjack writer: %v", err)
		}
	}()

	handler, err := slogcp.NewHandler(nil,
		slogcp.WithRedirectWriter(rolling),
		slogcp.WithSeverityAliases(false),
		slogcp.WithLevel(slog.LevelInfo),
	)
	if err != nil {
		log.Fatalf("create handler: %v", err)
	}
	defer func() {
		if err := handler.Close(); err != nil {
			log.Printf("handler close: %v", err)
		}
	}()

	logger := slog.New(handler)

	logger.Info("rotating file logger ready", slog.String("path", rolling.Filename))
	for i := range 5 {
		logger.Info("processing event", slog.Int("index", i))
	}

	// Trigger a manual rotation to highlight lumberjack's API surface.
	if err := rolling.Rotate(); err != nil {
		logger.Error("rotate log file", slog.Any("error", err))
	} else {
		logger.Info("log rotation complete")
	}
}
