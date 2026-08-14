module github.com/pjscruggs/slogcp/examples/grpc

go 1.26.5

require (
	google.golang.org/grpc v1.83.0
	google.golang.org/grpc/examples v0.0.0-20260813201637-bf9e7cd3430d
)

require github.com/pjscruggs/slogcp v0.0.0-unpublished

require (
	cloud.google.com/go/compute/metadata v0.9.0 // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/propagator v0.59.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.70.0 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260810153831-ec0a7760b754 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/pjscruggs/slogcp => ../..
