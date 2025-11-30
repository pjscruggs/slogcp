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

// Command basic illustrates a minimal slogcp usage pattern that writes logs to stdout.
//
// This example is both documentation, and a test for `slogcp`.
// Our Github workflow tests if any changes to `slogcp` break the example.
package main

import (
	"log"
	"log/slog"
	"os"

	"github.com/pjscruggs/slogcp"
)

// main runs the basic slogcp stdout example.
func main() {
	handler, err := slogcp.NewHandler(os.Stdout)
	if err != nil {
		log.Fatalf("failed to create slogcp handler: %v", err)
	}
	defer handler.Close()

	slog.New(handler).Info("service ready")
}
