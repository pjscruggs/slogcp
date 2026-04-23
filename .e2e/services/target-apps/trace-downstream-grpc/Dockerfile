# syntax=docker/dockerfile:1.20
# Build stage
ARG GO_VERSION
ARG DEBIAN_CODENAME
ARG DISTROLESS_TAG
FROM golang:${GO_VERSION}-${DEBIAN_CODENAME} AS builder

ENV GOTOOLCHAIN=auto

WORKDIR /build

# Pre-fetch generated module and workspace metadata to leverage Docker layer caching
COPY go.mod go.sum go.work* ./
COPY slogcp/go.mod slogcp/go.sum ./slogcp/
COPY traceproto/go.mod traceproto/go.sum ./traceproto/
COPY slogcp-grpc-adapter/go.mod slogcp-grpc-adapter/go.sum ./slogcp-grpc-adapter/
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Copy the remaining build context
COPY . .

# Refresh module metadata after copying service sources
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Verify the module graph after context copy
RUN --mount=type=cache,target=/go/pkg/mod go mod verify

ARG APP_VERSION=dev
ARG BUILD_TIME=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-w -s -X main.appVersion=${APP_VERSION} -X main.buildTime=${BUILD_TIME}" \
    -o trace-downstream-grpc .

FROM gcr.io/distroless/static-${DISTROLESS_TAG}:nonroot

WORKDIR /app

COPY --from=builder /build/trace-downstream-grpc .

USER 65532

EXPOSE 8080

ENTRYPOINT ["/app/trace-downstream-grpc"]
