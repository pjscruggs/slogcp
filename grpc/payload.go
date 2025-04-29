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

package grpc

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pjscruggs/slogcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// logPayload handles the logic for marshalling, truncating, and logging a message payload.
// It's called by stream wrappers and unary interceptors when payload logging is enabled.
func logPayload(ctx context.Context, logger *slogcp.Logger, opts *options, direction string, m any) {
	// Check if the message is a proto.Message.
	p, ok := m.(proto.Message)
	if !ok {
		// Log that we couldn't process the payload type.
		logger.LogAttrs(ctx, slog.LevelDebug,
			fmt.Sprintf("gRPC payload %s (non-proto)", direction),
			slog.String(payloadDirectionKey, direction),
			slog.String(payloadTypeKey, fmt.Sprintf("%T", m)),
		)
		return
	}

	// Marshal the proto message to JSON. Use MarshalOptions for stability.
	marshalOpts := protojson.MarshalOptions{
		Multiline:       false, // Keep it compact
		Indent:          "",
		AllowPartial:    true, // Attempt to marshal even if message is incomplete
		UseProtoNames:   true, // Use field names from .proto file
		UseEnumNumbers:  false,
		EmitUnpopulated: false, // Don't emit fields with zero values
	}
	jsonBytes, err := marshalOpts.Marshal(p)
	if err != nil {
		// Log the marshalling error itself.
		logger.LogAttrs(ctx, slog.LevelWarn, // Use Warn for internal logging issues
			"Failed to marshal gRPC payload for logging",
			slog.String(payloadDirectionKey, direction),
			slog.String(payloadTypeKey, fmt.Sprintf("%T", p)),
			slog.Any("error", err), // Use standard "error" key
		)
		// Log the event but indicate payload failure.
		logger.LogAttrs(ctx, slog.LevelDebug,
			fmt.Sprintf("gRPC payload %s (marshal error)", direction),
			slog.String(payloadDirectionKey, direction),
			slog.String(payloadTypeKey, fmt.Sprintf("%T", p)),
		)
		return
	}

	payloadStr := string(jsonBytes)
	originalSize := len(payloadStr)
	wasTruncated := false

	// Truncate if necessary based on options.
	if opts.maxPayloadLogSize > 0 && originalSize > opts.maxPayloadLogSize {
		payloadStr = payloadStr[:opts.maxPayloadLogSize]
		wasTruncated = true
	}

	// Build attributes for the payload log entry.
	attrs := []slog.Attr{
		slog.String(payloadDirectionKey, direction),
		slog.String(payloadTypeKey, fmt.Sprintf("%T", p)),
		slog.Bool(payloadTruncatedKey, wasTruncated),
	}
	if wasTruncated {
		attrs = append(attrs,
			slog.Int(payloadOriginalSizeKey, originalSize),
			slog.String(payloadPreviewKey, payloadStr), // Use preview key for truncated
		)
	} else {
		attrs = append(attrs, slog.String(payloadKey, payloadStr)) // Use main key for full
	}

	// Log the payload details at Debug level.
	logger.LogAttrs(ctx, slog.LevelDebug,
		fmt.Sprintf("gRPC payload %s", direction),
		attrs...,
	)
}
