module github.com/pjscruggs/slogcp/v2/examples/grpc

go 1.27.1

require (
	github.com/pjscruggs/slogcp/v2 v2.0.0-unpublished
	google.golang.org/grpc v1.83.2
	google.golang.org/grpc/examples v0.0.0-20260928104945-bf88ff499261
)

require (
	cloud.google.com/go/compute/metadata v0.10.0 // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/propagator v0.62.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.72.0 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261005182115-fad411399dd8 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/pjscruggs/slogcp/v2 => ../..
