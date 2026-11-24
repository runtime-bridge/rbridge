// Package buildinfo reports the version of the running binary.
package buildinfo

import (
	"runtime"
	"runtime/debug"
)

// Version is set at build time with -ldflags "-X github.com/rbridge/rbridge/internal/buildinfo.Version=v0.1.0".
var Version = ""

// Info describes the build.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Modified  bool   `json:"modified,omitempty"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

// Get returns the build information, falling back to module and VCS data.
func Get() Info {
	info := Info{Version: Version, GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH}
	bi, ok := debug.ReadBuildInfo()
	if ok {
		if info.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			info.Version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				info.Commit = s.Value
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
	}
	if info.Version == "" {
		info.Version = "dev"
	}
	return info
}
