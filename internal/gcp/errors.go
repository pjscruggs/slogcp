package gcp

import "errors"

// ErrProjectIDMissing indicates that the required GOOGLE_CLOUD_PROJECT
// environment variable was not set when attempting to load the configuration.
// The Cloud Logging client cannot be initialized without a project ID.
var ErrProjectIDMissing = errors.New("gcp: GOOGLE_CLOUD_PROJECT environment variable not set")

// ErrClientInitializationFailed indicates that an error occurred during the
// creation of the underlying `cloud.google.com/go/logging` client, often
// due to authentication issues or network problems connecting to GCP APIs.
// The original error from the client library is typically wrapped.
var ErrClientInitializationFailed = errors.New("gcp: cloud logging client initialization failed")

// ErrClientNotInitialized indicates that an operation requiring an initialized
// Cloud Logging client (like GetLogger or Flush via Close) was attempted
// before the client was successfully initialized or after initialization failed.
var ErrClientNotInitialized = errors.New("gcp: cloud logging client not initialized")
