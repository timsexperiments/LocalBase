package version

import "testing"

func TestResolve(t *testing.T) {
	vcs := map[string]string{"vcs.revision": "0123456789abcdef", "vcs.time": "2026-11-02T10:00:00Z", "vcs.modified": "true"}
	for name, c := range map[string]struct {
		commit, date string
		vcs          map[string]string
		want         Build
	}{
		"stamped wins over version control": {"aaaa", "2026-01-01", vcs, Build{"1.2.3", "aaaa", "2026-01-01", "go1", false}},
		"version control fills the gaps":    {"", "", vcs, Build{"1.2.3", "0123456789abcdef", "2026-11-02T10:00:00Z", "go1", true}},
		"neither says unknown":              {"", "", nil, Build{"1.2.3", "unknown", "unknown", "go1", false}},
	} {
		if got := resolve("1.2.3", c.commit, c.date, "go1", c.vcs); got != c.want {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestString(t *testing.T) {
	b := Build{Version: "1.2.3", Commit: "0123456789abcdef", Date: "2026-11-02", GoVersion: "go1.26.0"}
	if got, want := b.String(), "1.2.3 (commit 0123456789ab, 2026-11-02, go1.26.0)"; got != want {
		t.Errorf("%q", got)
	}
	b.Modified = true
	if got, want := b.String(), "1.2.3 (commit 0123456789ab+modified, 2026-11-02, go1.26.0)"; got != want {
		t.Errorf("%q", got)
	}
}

// Only a stamped, unmodified build is a release.
func TestReleaseBuild(t *testing.T) {
	old := Commit
	t.Cleanup(func() { Commit = old })
	Commit = ""
	if ReleaseBuild() {
		t.Error("an unstamped build is a release")
	}
	Commit = "0123456789abcdef"
	if got, want := ReleaseBuild(), !Get().Modified; got != want {
		t.Errorf("a stamped build: release %v, want %v (modified %v)", got, want, Get().Modified)
	}
}
