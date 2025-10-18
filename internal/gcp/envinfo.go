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

import (
	"os"
	"strings"
	"sync"
)

// RuntimeInfo captures metadata about the current Google Cloud environment.
type RuntimeInfo struct {
	Labels         map[string]string
	ServiceContext map[string]string
}

var (
	runtimeInfo     RuntimeInfo
	runtimeInfoOnce sync.Once
)

// DetectRuntimeInfo inspects well-known environment variables to infer
// platform-specific labels and service context. Results are cached for reuse.
func DetectRuntimeInfo() RuntimeInfo {
	runtimeInfoOnce.Do(func() {
		runtimeInfo = detectRuntimeInfo()
	})
	return runtimeInfo
}

func detectRuntimeInfo() RuntimeInfo {
	var info RuntimeInfo

	service := strings.TrimSpace(os.Getenv("K_SERVICE"))
	revision := strings.TrimSpace(os.Getenv("K_REVISION"))
	config := strings.TrimSpace(os.Getenv("K_CONFIGURATION"))

	// Cloud Run (services)
	if service != "" && revision != "" {
		info.ServiceContext = map[string]string{
			"service": service,
			"version": revision,
		}
		labels := map[string]string{
			"cloud_run.service":  service,
			"cloud_run.revision": revision,
		}
		if config != "" {
			labels["cloud_run.configuration"] = config
		}
		info.Labels = labels
		return info
	}

	// Cloud Functions (Gen 2 shares K_SERVICE)
	if service != "" && strings.TrimSpace(os.Getenv("FUNCTION_TARGET")) != "" {
		info.ServiceContext = map[string]string{
			"service": service,
		}
		if revision != "" {
			info.ServiceContext["version"] = revision
		}
		info.Labels = map[string]string{
			"cloud_function.name": service,
		}
		return info
	}

	return info
}
