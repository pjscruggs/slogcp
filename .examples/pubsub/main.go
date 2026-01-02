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

// Command pubsub demonstrates slogcppubsub trace propagation and per-message logging.
//
// This example is both documentation, and a test for `slogcp`.
// Our Github workflow tests if any changes to `slogcp` break the example.
package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"time"

	"cloud.google.com/go/pubsub/v2"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/pjscruggs/slogcp"
	"github.com/pjscruggs/slogcp/slogcppubsub"
)

// main runs the Pub/Sub logging example.
func main() {
	ctx := context.Background()

	tracerProvider := sdktrace.NewTracerProvider()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := tracerProvider.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown tracer provider: %v", err)
		}
	}()

	handler, err := slogcp.NewHandler(os.Stdout)
	if err != nil {
		log.Fatalf("failed to create handler: %v", err)
	}
	defer func() {
		if cerr := handler.Close(); cerr != nil {
			log.Printf("handler close: %v", cerr)
		}
	}()

	logger := slog.New(handler)

	publishCtx, span := tracerProvider.Tracer("example/pubsub").Start(ctx, "publish")
	defer span.End()

	msg := &pubsub.Message{
		Data: []byte("hello"),
	}

	slogcppubsub.Inject(publishCtx, msg)

	// Simulate server-assigned fields for the local demo.
	msg.ID = "msg-123"

	receive := slogcppubsub.WrapReceiveHandler(
		// handler simulates subscriber processing with a derived logger.
		func(ctx context.Context, msg *pubsub.Message) {
			slogcp.Logger(ctx).Info("processing message", "payload", string(msg.Data))
			msg.Ack()
		},
		slogcppubsub.WithLogger(logger),
		slogcppubsub.WithSubscriptionID("orders-sub"),
		slogcppubsub.WithTopicID("orders"),
		slogcppubsub.WithTracerProvider(tracerProvider),
		slogcppubsub.WithLogMessageID(true),
	)

	receive(ctx, msg)
}
