// Package buildinfo carries the values stamped into the binary at build time.
//
// They are variables rather than constants so the linker can set them:
//
//	go build -ldflags "-X github.com/shaowenchen/applab/internal/buildinfo.Version=1.2.3"
package buildinfo

// Version is the release version, "dev" for a local build.
var Version = "dev"

// Commit is the git revision the binary was built from.
var Commit = "unknown"

// BuildTime is when the binary was built, as a UTC timestamp string.
var BuildTime = "unknown"
