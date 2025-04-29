package slogcp

import "fmt"

// Version is the current version of the slogcp library.
// Follows semantic versioning (https://semver.org/).
// It can be overridden at build time via -ldflags.
var Version = "v0.0.0-dev"

// UserAgent is the string sent with Cloud Logging API requests, identifying this library.
var UserAgent string

func init() {
    // Initialize UserAgent using the (possibly injected) Version
    UserAgent = fmt.Sprintf("slogcp/%s", Version)
}

func GetVersion() string { return Version }
