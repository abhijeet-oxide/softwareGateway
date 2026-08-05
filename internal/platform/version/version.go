// Package version carries build information stamped in at link time.
//
// See docs/design/12-observability-and-audit.md section 2.7 — the build_info
// metric is how a dashboard correlates a behaviour change with a deployment.
package version

import (
	"fmt"
	"runtime"
)

// Stamped by -ldflags at build time. See the Makefile and Dockerfiles.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// APIVersion is the served API version. Independent of the build version:
// many builds serve one API version. See docs/design/09-api.md section 11.
const APIVersion = "v1"

// Info is the full build identity, returned by GET /api/v1/system/version.
// These four fields are what is needed to interpret a bug report.
type Info struct {
	Version    string `json:"version"`
	Commit     string `json:"commit"`
	BuildDate  string `json:"buildDate"`
	GoVersion  string `json:"goVersion"`
	APIVersion string `json:"apiVersion"`
	Component  string `json:"component"`
}

// Get returns the build identity for the named component
// ("coordinator", "worker", "transferctl").
func Get(component string) Info {
	return Info{
		Version:    Version,
		Commit:     Commit,
		BuildDate:  Date,
		GoVersion:  runtime.Version(),
		APIVersion: APIVersion,
		Component:  component,
	}
}

// String renders a one-line summary for CLI output and startup logs.
func (i Info) String() string {
	return fmt.Sprintf("%s %s (%s, built %s, %s)",
		i.Component, i.Version, i.Commit, i.BuildDate, i.GoVersion)
}
