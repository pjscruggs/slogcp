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
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Copy the remaining build context (service sources, optional slogcp replace)
COPY . .

# Refresh the workspace download cache after copying sources
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Verify module graph after context copy
RUN --mount=type=cache,target=/go/pkg/mod go mod verify

# Build arguments for versioning
ARG APP_VERSION=dev
ARG BUILD_TIME=unknown

# Build the application with cached compiler artifacts
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-w -s -X main.appVersion=${APP_VERSION} -X main.buildTime=${BUILD_TIME}" \
    -o core-logging-target-app \
    .

# Runtime stage using distroless
FROM gcr.io/distroless/static-${DISTROLESS_TAG}:nonroot

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /build/core-logging-target-app .

# The nonroot user has UID 65532
USER 65532

EXPOSE 8080

ENTRYPOINT ["/app/core-logging-target-app"]
