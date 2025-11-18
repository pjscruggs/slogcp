package main

import (
	"bytes"
	"context"
	"testing"
)

func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf); err != nil {
		t.Fatalf("run returned error: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatalf("expected log output, got none")
	}
}

func BenchmarkRun(b *testing.B) {
	ctx := context.Background()
	scenario, err := newClientScenario()
	if err != nil {
		b.Fatalf("new client scenario: %v", err)
	}
	defer scenario.Close()

	b.ResetTimer()
	for b.Loop() {
		var buf bytes.Buffer
		if err := scenario.Run(ctx, &buf); err != nil {
			b.Fatalf("scenario run returned error: %v", err)
		}
	}
}
