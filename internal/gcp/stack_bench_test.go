package gcp

import (
	"runtime"
	"testing"
)

var (
	benchStackString   string
	benchTopFrame      runtime.Frame
	benchFallbackStack string
)

func BenchmarkCaptureStack(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchStackString, benchTopFrame = CaptureStack(nil)
	}
	if benchStackString == "" {
		b.Fatal("empty stack trace from CaptureStack")
	}
	_ = benchTopFrame
}

func BenchmarkCaptureStackSkipInternal(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchStackString, benchTopFrame = CaptureStack(SkipInternalStackFrame)
	}
	if benchStackString == "" {
		b.Fatal("empty stack trace with SkipInternalStackFrame")
	}
	_ = benchTopFrame
}

func BenchmarkCaptureAndFormatFallbackStack(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchFallbackStack = captureAndFormatFallbackStack()
	}
	if benchFallbackStack == "" {
		b.Fatal("empty fallback stack trace")
	}
}
