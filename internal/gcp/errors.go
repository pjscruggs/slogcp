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
