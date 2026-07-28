// Package buildinfo carries the one build-time variable the rest of the
// program needs. It exists as its own package purely so the -ldflags path
// (hopreact/internal/buildinfo.Version) is stable and doesn't depend on
// where main happens to live.
package buildinfo

// Version is the running build's version, injected at link time by the
// release workflow from the git tag:
//
//	-ldflags "-X hopreact/internal/buildinfo.Version=v1.2.3"
//
// Left as "dev" for local builds, which is exactly what an un-tagged
// working copy should report.
var Version = "dev"
