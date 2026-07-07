package main

import "runtime/debug"

// Version is set from runtime/debug.ReadBuildInfo at package init time.
// When built with "go install" or "go build" from a module root, it will be
// the module version (e.g. "v1.0.0"). When built locally it will be "(devel)".
// Use -ldflags "-X main.Version=custom" to override at link time.
var Version string

func init() {
	Version = versionFromBuildInfo()
}

func versionFromBuildInfo() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	v := info.Main.Version
	if v != "" && v != "(devel)" {
		return v
	}
	// If we have a VCS revision, use that instead of "(devel)".
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			rev := s.Value
			if len(rev) > 8 {
				rev = rev[:8]
			}
			return "dev-" + rev
		}
	}
	return "dev"
}
