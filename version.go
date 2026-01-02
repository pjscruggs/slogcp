// Copyright 2025-2026 Patrick J. Scruggs
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
var Version = "v1.1.1"

// UserAgent identifies this library when helper components need to report a
// version string. It is initialized from Version and can be overridden at
// build time.
var UserAgent string

// init seeds UserAgent with the current Version value.
func init() {
	// Initialize UserAgent using the (possibly injected) Version
	UserAgent = fmt.Sprintf("slogcp/%s", Version)
}

// GetVersion returns the current library version string.
func GetVersion() string { return Version }
