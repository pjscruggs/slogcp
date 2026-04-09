# syntax=docker/dockerfile:1.20
# Build stage
ARG GO_VERSION
ARG DEBIAN_CODENAME
ARG DISTROLESS_TAG
FROM golang:${GO_VERSION}-${DEBIAN_CODENAME} AS builder

ENV GOTOOLCHAIN=auto

WORKDIR /build

# Pre-fetch module dependencies to leverage Docker layer caching
COPY go.mod ./
COPY slogcp ./slogcp
COPY traceproto ./traceproto
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Copy the remaining build context (service sources and generated assets)
COPY . .

# Ensure go.mod/go.sum include all required packages after copying sources
RUN --mount=type=cache,target=/go/pkg/mod go mod tidy

# Refresh module metadata after copying service sources
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Verify the module graph after context copy
RUN --mount=type=cache,target=/go/pkg/mod go mod verify

# Build arguments for versioning
ARG APP_VERSION=dev
ARG BUILD_TIME=unknown

# Build the application binary with cached compiler artifacts
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-w -s -X main.appVersion=${APP_VERSION} -X main.buildTime=${BUILD_TIME}" \
    -o trace-target-app .

# Runtime stage using distroless
FROM gcr.io/distroless/static-${DISTROLESS_TAG}:nonroot

WORKDIR /app

COPY --from=builder /build/trace-target-app .

USER 65532

EXPOSE 8080

ENTRYPOINT ["/app/trace-target-app"]
