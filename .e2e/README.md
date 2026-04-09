# `.e2e`

`.e2e` is the GitHub CI end-to-end suite that confirms `slogcp` and its automatic dependency updates still behave correctly in Google Cloud. This directory contains the Cloud Run target apps, the harness that drives them, and the Cloud Build config for the full suite. 

The CI deployment path keeps those Cloud Run services private. The build and harness grant `roles/run.invoker` only to the service accounts that participate in the test run, and the harness plus trace target app use Google-signed ID tokens for service-to-service calls instead of exposing the services publicly.

## What this suite tests

### Cloud Logging Severity And Payload Shape

The logging suite checks severity mapping across `DEFAULT`, `DEBUG`, `INFO`, `NOTICE`, `WARNING`, `ERROR`, `CRITICAL`, `ALERT`, and `EMERGENCY`. It also checks that structured and nested payloads survive ingestion, that timestamps are recorded, and that batch logging produces the expected set of entries.

### Cloud Logging Metadata

The suite checks metadata that has to land in the right place for Cloud Logging and Error Reporting to stay useful: `serviceContext`, resource labels, custom labels, operation grouping, source location, startup metadata, and the expected application log name `run.googleapis.com/stdout`.

### `httpRequest` Ingestion Behavior

The suite checks finalized request logs, in-flight request logs, and the way Google Cloud ingests `httpRequest` fields. That includes the expected response metrics on finalized logs, the omission of those response metrics while a request is still in flight, and diagnostic probes for backend handling of unknown `httpRequest` keys and `httpRequest.status=0`.

### Single-Service Trace Correlation

The trace suite checks the baseline case first: a traced request should produce log entries with the correct Cloud Trace resource name and should produce the expected span in Cloud Trace.

### Cross-Service Trace Propagation

The suite checks trace propagation across downstream HTTP calls, downstream unary gRPC calls, and Pub/Sub publish/consume flows. The assertions require logs from the participating services to agree on the trace and require Cloud Trace to contain the expected spans for the full request path.

### gRPC Streaming Trace Propagation

The streaming tests cover downstream gRPC server-stream, client-stream, and bidirectional-stream flows. They check both log correlation and the expected Cloud Trace spans for the full streaming exchange.

### Adapter Logging

The suite checks `slogcp-grpc-adapter` behavior inside the gRPC path. Those assertions verify that interceptor logs keep trace correlation and that adapter-emitted severity and gRPC method/service metadata survive ingestion.

### Target-App Startup Metadata

The core logging suite checks the `Target app starting` log entry emitted by the deployed service. The harness filters by `resource.labels.service_name` and then verifies that the startup payload reports the expected `version` for the active scenario. That is what backs the default `dev` case, the `custom-app-version` override, and the startup-focused part of the `missing-adc` scenario.

### Trace-Target Startup Configuration

The trace suite checks the `trace-target-app starting` log entry emitted by the trace target service. The harness verifies the configured `downstream_http` and `downstream_grpc` values and then checks the startup booleans `disable_http_trace_propagation`, `disable_grpc_trace_propagation`, and `default_trace_sampled`. Those assertions are what back the `trace-disable-http-propagation`, `trace-disable-grpc-propagation`, and `trace-default-unsampled` scenarios.
