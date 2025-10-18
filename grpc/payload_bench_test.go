package grpc

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"
)

var (
	benchMetadata metadata.MD
	benchLogger   = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
)

func buildStructMessage() *structpb.Struct {
	return &structpb.Struct{
		Fields: map[string]*structpb.Value{
			"user": structpb.NewStructValue(&structpb.Struct{
				Fields: map[string]*structpb.Value{
					"id":       structpb.NewStringValue("user-12345"),
					"email":    structpb.NewStringValue("user@example.com"),
					"flags":    structpb.NewListValue(&structpb.ListValue{Values: []*structpb.Value{structpb.NewStringValue("beta"), structpb.NewStringValue("mobile")}}),
					"metadata": structpb.NewStructValue(&structpb.Struct{Fields: map[string]*structpb.Value{"account_age": structpb.NewNumberValue(972.0)}}),
				},
			}),
			"session": structpb.NewStructValue(&structpb.Struct{
				Fields: map[string]*structpb.Value{
					"id":        structpb.NewStringValue("sess-abc"),
					"createdAt": structpb.NewStringValue("2025-10-18T12:34:56Z"),
					"scopes": structpb.NewListValue(&structpb.ListValue{Values: []*structpb.Value{
						structpb.NewStringValue("read"),
						structpb.NewStringValue("write"),
						structpb.NewStringValue("admin"),
					}}),
				},
			}),
			"payload": structpb.NewStructValue(&structpb.Struct{
				Fields: map[string]*structpb.Value{
					"count": structpb.NewNumberValue(128.0),
					"items": structpb.NewListValue(&structpb.ListValue{
						Values: []*structpb.Value{
							structpb.NewStringValue(strings.Repeat("item-", 16)),
							structpb.NewStringValue(strings.Repeat("payload-", 12)),
						},
					}),
				},
			}),
		},
	}
}

func BenchmarkLogPayload(b *testing.B) {
	message := buildStructMessage()
	ctx := context.Background()

	b.Run("FullPayload", func(b *testing.B) {
		opts := &options{
			logPayloads:       true,
			maxPayloadLogSize: defaultMaxPayloadLogSize,
		}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			logPayload(ctx, benchLogger, opts, "sent", message)
		}
	})

	b.Run("TruncatedPayload", func(b *testing.B) {
		opts := &options{
			logPayloads:       true,
			maxPayloadLogSize: 128,
		}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			logPayload(ctx, benchLogger, opts, "received", message)
		}
	})
}

func BenchmarkFilterMetadata(b *testing.B) {
	md := metadata.MD{
		"authorization":       {"Bearer very-long-token-value-that-should-be-filtered"},
		"x-custom-header":     {"alpha", "bravo", "charlie"},
		"x-enriched-metadata": {"foo=bar", "baz=qux", "trace=abc123"},
		"content-type":        {"application/grpc"},
		"user-agent":          {"grpc-go/1.76.0"},
		"grpc-trace-bin":      {"\x00\x01\x02\x03"},
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchMetadata = filterMetadata(md, defaultMetadataFilter)
	}
	if len(benchMetadata) == 0 {
		b.Fatal("metadata unexpectedly filtered to zero entries")
	}
}
