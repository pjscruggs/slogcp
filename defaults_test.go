package slogcp_test

import "github.com/pjscruggs/slogcp"

// prefersManagedDefaults reports whether runtime detection mirrors managed GCP
// platforms where slogcp opts into Cloud Logging defaults (short severity names,
// omitted time field).
func prefersManagedDefaults() bool {
	switch slogcp.DetectRuntimeInfo().Environment {
	case slogcp.RuntimeEnvCloudRunService,
		slogcp.RuntimeEnvCloudRunJob,
		slogcp.RuntimeEnvCloudFunctions,
		slogcp.RuntimeEnvAppEngineStandard,
		slogcp.RuntimeEnvAppEngineFlexible:
		return true
	default:
		return false
	}
}
