# syntax=docker/dockerfile:1.20
# Build stage
ARG GO_VERSION
ARG DEBIAN_CODENAME
ARG DISTROLESS_TAG
FROM golang:${GO_VERSION}-${DEBIAN_CODENAME} AS builder

ENV GOTOOLCHAIN=auto
WORKDIR /build
# Copy generated module and workspace metadata first to maximize cache reuse.
COPY go.mod go.sum go.work* ./
COPY traceproto/go.mod traceproto/go.sum ./traceproto/
# Download dependencies early to warm the module cache.
RUN --mount=type=cache,target=/go/pkg/mod go mod download
# Copy the remaining source
COPY . .
# Build arguments for versioning
ARG APP_VERSION=dev
ARG BUILD_TIME=unknown
# Build the application
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -ldflags "-w -s -X main.version=${APP_VERSION} -X main.buildTime=${BUILD_TIME}" -o e2e-harness .
# Runtime stage using distroless
FROM gcr.io/distroless/base-${DISTROLESS_TAG}:nonroot
WORKDIR /app
# Copy the binary from builder
COPY --from=builder /build/e2e-harness .
# The nonroot user has UID 65532
USER 65532
ENTRYPOINT ["/app/e2e-harness"]
