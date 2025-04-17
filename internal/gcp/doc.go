// Package gcp contains internal types and functions for interacting with
// Google Cloud Platform services, specifically Cloud Logging.
//
// This package is not intended for direct use by consumers of the slogcp
// library. It encapsulates the logic for managing the Cloud Logging client
// lifecycle, loading configuration from the environment, mapping slog levels
// to GCP severity, formatting log entries including trace and source location
// information, and handling structured payloads and errors according to GCP
// conventions.
package gcp
