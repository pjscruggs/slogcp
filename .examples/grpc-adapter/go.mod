module github.com/pjscruggs/slogcp/v2/examples/grpc-adapter

go 1.27.1

require (
	github.com/grpc-ecosystem/go-grpc-middleware/v2 v2.3.4
	github.com/pjscruggs/slogcp-grpc-adapter/v2 v2.0.0-20260912111532-1cd3ce824a74
	github.com/pjscruggs/slogcp/v2 v2.0.0-unpublished
	google.golang.org/grpc v1.83.2
)

require (
	cloud.google.com/go/compute/metadata v0.9.0 // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/propagator v0.62.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.46.0 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.opentelemetry.io/otel/trace v1.46.0 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260911204522-f61a6ca850bd // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/pjscruggs/slogcp/v2 => ../..
