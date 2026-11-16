package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tarEntry struct {
	name, link, body string
	dir              bool
}

func writeTarGz(t *testing.T, path string, entries []tarEntry) {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o755}
		switch {
		case e.dir:
			h.Typeflag = tar.TypeDir
		case e.link != "":
			h.Typeflag, h.Linkname = tar.TypeSymlink, e.link
		default:
			h.Typeflag, h.Size = tar.TypeReg, int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// extracting unpacks entries into a package directory that has a directory called outside beside it.
func extracting(t *testing.T, entries []tarEntry) (dir, outside string, err error) {
	t.Helper()
	parent := t.TempDir()
	if parent, err = filepath.EvalSymlinks(parent); err != nil {
		t.Fatal(err)
	}
	dir, outside = filepath.Join(parent, "pkg"), filepath.Join(parent, "outside")
	for _, d := range []string{dir, outside} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(parent, "a.tar.gz")
	writeTarGz(t, archive, entries)
	return dir, outside, extractTarGz(archive, dir, 0)
}

func TestAPackageMayHoldItsOwnLinks(t *testing.T) {
	dir, _, err := extracting(t, []tarEntry{
		{name: "Versions/A/", dir: true},
		{name: "Versions/A/lib", body: "the library"},
		{name: "Versions/Current", link: "A"},
		{name: "lib", link: "Versions/Current/lib"},
		{name: "bin/tool", link: "../lib"},
		{name: "nowhere", link: "not-in-this-package"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lib", "bin/tool"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(b) != "the library" {
			t.Errorf("%s: %q %v", name, b, err)
		}
	}
}

// Each link's target looks inside the package when read as written, and only following the links
// already made shows where it really goes.
func TestALinkCannotCarryAnEntryOutOfThePackage(t *testing.T) {
	first := []tarEntry{
		{name: "top/", dir: true},
		{name: "x/y/", dir: true},
		{name: "x/y/up", link: "../../top"},
	}
	for _, c := range []struct {
		name    string
		entries []tarEntry
	}{
		{"a file written through two links", []tarEntry{
			{name: "x/y/up/q", link: "../../outside"},
			{name: "x/y/up/q/planted", body: "written outside"},
		}},
		{"a link made through a link", []tarEntry{
			{name: "x/y/up/q", link: "../../outside"},
		}},
		{"a directory made through a link", []tarEntry{
			{name: "x/y/up/new/", dir: true},
		}},
		{"a file written through one link", []tarEntry{
			{name: "x/y/up/planted", body: "written beside the link's real target"},
		}},
		{"a link whose target goes through a link", []tarEntry{
			{name: "esc", link: "x/y/up/../../outside"},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, outside, err := extracting(t, append(append([]tarEntry{}, first...), c.entries...))
			if err == nil {
				t.Error("the archive was accepted")
			}
			names, rerr := os.ReadDir(outside)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if len(names) != 0 {
				t.Errorf("%d files outside the package", len(names))
			}
		})
	}
}

// A package reached by a path with a link in it, like /tmp on a Mac, is no different.
func TestLinksAreJudgedFromWhereThePackageReallyIs(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(parent, "real", "pkg"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(parent, "named")); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(parent, "a.tar.gz")
	writeTarGz(t, archive, []tarEntry{{name: "lib", body: "the library"}, {name: "alias", link: "lib"}})
	if err := extractTarGz(archive, filepath.Join(parent, "named", "pkg"), 0); err != nil {
		t.Error(err)
	}
}

// A disk that doesn't care about case mustn't let an entry through a link by spelling it differently.
func TestACaseVariantCannotGoThroughALink(t *testing.T) {
	_, outside, _ := extracting(t, []tarEntry{
		{name: "top/", dir: true},
		{name: "x/y/", dir: true},
		{name: "x/y/up", link: "../../top"},
		{name: "X/Y/UP/q", link: "../../outside"},
		{name: "X/Y/UP/Q/planted", body: "written outside"},
	})
	names, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("%d files outside the package", len(names))
	}
}

// Links stay checked as they're made, and a link to the package itself is one that leaves.
func TestALinkToThePackageItselfIsRefused(t *testing.T) {
	_, _, err := extracting(t, []tarEntry{
		{name: "top/", dir: true},
		{name: "x/y/", dir: true},
		{name: "x/y/up", link: "../../top"},
		{name: "root", link: "x/y/up/.."},
	})
	if err == nil {
		t.Error("the archive was accepted")
	}
}

func TestLinksThatPlainlyLeaveAreRefused(t *testing.T) {
	for _, link := range []string{"/etc", "..", "../outside", "a/../../outside"} {
		if _, _, err := extracting(t, []tarEntry{{name: "esc", link: link}}); err == nil {
			t.Errorf("%q was accepted", link)
		}
	}
}

func TestALoopOfLinksIsRefused(t *testing.T) {
	_, _, err := extracting(t, []tarEntry{{name: "a", link: "b"}, {name: "b", link: "a"}})
	if err == nil || !strings.Contains(err.Error(), "a") {
		t.Errorf("the error: %v", err)
	}
}
