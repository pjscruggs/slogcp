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

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"time"

	"cloud.google.com/go/logging"
	"github.com/pjscruggs/slogcp-e2e-internal/services/target-apps/core-logging-target-app/handlers"
	slogcpgrpc "github.com/pjscruggs/slogcp-grpc"
	"github.com/pjscruggs/slogcp/v2"
	"go.opentelemetry.io/otel/trace"
)

// logGRPCAPI proves buffered API delivery and reports all shutdown failures.
func logGRPCAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var request handlers.LogRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		http.Error(w, "invalid log request", http.StatusBadRequest)
		return
	}
	if request.TestID == "" {
		http.Error(w, "test_id required", http.StatusBadRequest)
		return
	}
	if err := emitGRPCAPI(r, request); err != nil {
		log.Printf("gRPC API proof failed %v", err)
		http.Error(w, "gRPC API delivery failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"success": true, "message": request.Message}); err != nil {
		log.Printf("gRPC API response failed %v", err)
	}
}

func emitGRPCAPI(r *http.Request, request handlers.LogRequest) (result error) {
	projectID := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if projectID == "" {
		return errors.New("GOOGLE_CLOUD_PROJECT is required for gRPC API proof")
	}
	client, err := logging.NewClient(r.Context(), projectID)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, client.Close()) }()
	client.OnError = func(err error) { log.Printf("gRPC API delivery error %v", err) }
	cloudLogger := client.Logger("slogcp-grpc-e2e",
		logging.DelayThreshold(time.Hour),
		logging.EntryCountThreshold(1000),
		logging.ConcurrentWriteLimit(2),
		logging.BufferedByteLimit(1<<20),
		logging.CommonLabels(map[string]string{"transport": "grpc"}),
		logging.ContextFunc(func() (context.Context, func()) {
			return context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
		}),
	)
	exporter, err := slogcpgrpc.NewExporter(cloudLogger)
	if err != nil {
		return err
	}
	handler, err := slogcp.NewHandlerWithExporter(exporter,
		slogcp.WithTraceProjectID(projectID),
		slogcp.WithSourceLocationEnabled(true),
	)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(request.TestID))
	var traceID trace.TraceID
	var spanID trace.SpanID
	copy(traceID[:], digest[:16])
	copy(spanID[:], digest[16:24])
	ctx := trace.ContextWithSpanContext(r.Context(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))
	var pc [1]uintptr
	runtime.Callers(1, pc[:])
	record := slog.NewRecord(time.Now(), slogcp.LevelNotice.Level(), request.Message, pc[0])
	record.AddAttrs(
		slog.String("test_id", request.TestID),
		slog.String("logging.googleapis.com/insertId", request.TestID),
		slog.Group("logging.googleapis.com/operation", "id", request.TestID, "producer", "grpc-e2e", "first", true, "last", true),
		slog.Group(slogcp.LabelsGroup, "proof", "grpc-api"),
		slog.Group("details", "count", 7, "complete", true),
		slog.Any("httpRequest", &slogcp.HTTPRequest{Request: r, Status: http.StatusOK, ResponseSize: 1, Latency: 5 * time.Millisecond}),
	)
	if err := handler.Handle(ctx, record); err != nil {
		return errors.Join(fmt.Errorf("export failed %w", err), handler.Close(), exporter.Flush())
	}
	return errors.Join(handler.Close(), exporter.Flush())
}
