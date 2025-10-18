// Copyright 2025 Patrick J. Scruggs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gcp

import "errors"

// ErrProjectIDMissing indicates that a GCP Project ID or Parent resource name
// (required when the log target is GCP) could not be determined from configuration
// options, environment variables, or the metadata server.
var ErrProjectIDMissing = errors.New("gcp: parent resource (e.g., projects/PROJECT_ID) required for GCP target but not found")

// ErrInvalidRedirectTarget indicates that the value for SLOGCP_REDIRECT_AS_JSON_TARGET
// was malformed or invalid.
var ErrInvalidRedirectTarget = errors.New("gcp: invalid redirect target")

// ErrClientInitializationFailed indicates that an error occurred during the
// creation or initialization of the underlying `cloud.google.com/go/logging` client.
// The original error from the client library is typically wrapped.
var ErrClientInitializationFailed = errors.New("gcp: cloud logging client initialization failed")

// ErrClientNotInitialized indicates that an operation requiring an initialized
// Cloud Logging client (like GetLogger or Flush) was attempted before the
// client was successfully initialized or after initialization failed.
var ErrClientNotInitialized = errors.New("gcp: cloud logging client not initialized")
