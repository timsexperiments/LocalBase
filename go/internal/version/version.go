// Package version is what this build says it is. A release stamps Version,
// Commit and Date with -ldflags -X (tools/build.sh); a plain `go build` from a
// clone reports the commit and time Go stamped from version control. Go does not
// stamp builds from a git worktree, so those say "unknown" unless built with
// tools/build.sh.
package version

import (
	"runtime"
	"runtime/debug"
	"sync"
)

// Stamped at build time. Version is the source of truth for the release number.
var (
	Version = "0.2.0"
	Commit  = ""
	Date    = ""
)

// Build is this binary's identity.
type Build struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	// Modified: built from a checkout with uncommitted changes.
	Modified bool `json:"modified,omitempty"`
}

// fromVCS reads what Go stamped from version control.
var fromVCS = sync.OnceValue(func() map[string]string {
	settings := map[string]string{}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			settings[s.Key] = s.Value
		}
	}
	return settings
})

// Get is this build, using version control's stamp for whatever was not stamped.
func Get() Build {
	return resolve(Version, Commit, Date, runtime.Version(), fromVCS())
}

func resolve(version, commit, date, goVersion string, vcs map[string]string) Build {
	b := Build{Version: version, Commit: commit, Date: date, GoVersion: goVersion}
	if b.Commit == "" {
		b.Commit = vcs["vcs.revision"]
		b.Modified = vcs["vcs.modified"] == "true"
	}
	if b.Date == "" {
		b.Date = vcs["vcs.time"]
	}
	if b.Commit == "" {
		b.Commit = "unknown"
	}
	if b.Date == "" {
		b.Date = "unknown"
	}
	return b
}

// String is one line for people: "0.2.0 (commit 0123456789ab, 2026-11-02T10:00:00Z, go1.26.0)".
func (b Build) String() string {
	commit := b.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	if b.Modified {
		commit += "+modified"
	}
	return b.Version + " (commit " + commit + ", " + b.Date + ", " + b.GoVersion + ")"
}

// ReleaseBuild: a release stamps its commit. A plain go build does not, and neither does a build
// from a checkout with uncommitted changes.
func ReleaseBuild() bool { return releaseBuild(Commit, fromVCS()) }

// releaseBuild is ReleaseBuild for a stamped commit and version control's stamp. Commit is an
// -ldflags -X target that Go code never assigns: a test passes its commit here instead.
func releaseBuild(commit string, vcs map[string]string) bool {
	return commit != "" && !resolve(Version, commit, Date, runtime.Version(), vcs).Modified
}
