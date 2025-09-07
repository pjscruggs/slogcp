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

package slogcp

import "fmt"

// Version is the current version of the slogcp library.
// Follows semantic versioning (https://semver.org/).
// It can be overridden at build time via -ldflags.
var Version = "v0.1.5-alpha"

// UserAgent is the string sent with Cloud Logging API requests, identifying this library.
// Initialized based on the Version variable.
var UserAgent string

func init() {
	// Initialize UserAgent using the (possibly injected) Version
	UserAgent = fmt.Sprintf("slogcp/%s", Version)
}

// GetVersion returns the current library version string.
func GetVersion() string { return Version }
