package main

import (
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// knownWithoutMockKeychain are the scripts that started Chrome without the flag when this
// check was written. #454 adds it to them: delete each line as that lands. Nothing else may
// join the list. LOCALBASE_CHROME_FLAGS_ALL=1 ignores it, to see what the check finds.
var knownWithoutMockKeychain = []string{
	"axe-check.sh",
	"chat-runs-check.sh",
	"mic-check.sh",
	"phone-check.sh",
	"ui-shots.sh",
}

var startsChrome = regexp.MustCompile(`--headless|--remote-debugging-(port|pipe)`)

// launchersWithoutMockKeychain lists the scripts under fsys that start a Chrome without
// --use-mock-keychain. A flag only in a comment does not count.
func launchersWithoutMockKeychain(fsys fs.FS) ([]string, error) {
	var missing []string
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if base := path.Base(name); base == "node_modules" || base == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, "_test.go") {
			return nil
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		switch path.Ext(name) {
		case ".sh", ".go", ".ts", ".js", ".mjs", ".py":
		case "":
			if !strings.HasPrefix(string(b), "#!") {
				return nil
			}
		default:
			return nil
		}
		var code strings.Builder
		for _, line := range strings.Split(string(b), "\n") {
			if t := strings.TrimSpace(line); !strings.HasPrefix(t, "#") && !strings.HasPrefix(t, "//") {
				code.WriteString(line + "\n")
			}
		}
		if startsChrome.MatchString(code.String()) && !strings.Contains(code.String(), "--use-mock-keychain") {
			missing = append(missing, name)
		}
		return nil
	})
	return missing, err
}

// Every script under tools that starts Chrome keeps it away from the Keychain: see
// tools/README.md for what happens otherwise.
func TestEveryChromeLauncherStaysAwayFromTheKeychain(t *testing.T) {
	missing, err := launchersWithoutMockKeychain(os.DirFS(".."))
	if err != nil {
		t.Fatal(err)
	}
	known := knownWithoutMockKeychain
	if os.Getenv("LOCALBASE_CHROME_FLAGS_ALL") != "" {
		known = nil
	}
	for _, name := range missing {
		if !slices.Contains(known, name) {
			t.Errorf("tools/%s starts Chrome without --use-mock-keychain --password-store=basic; see tools/README.md", name)
		}
	}
	for _, name := range known {
		if !slices.Contains(missing, name) {
			t.Errorf("tools/%s no longer starts Chrome without the flag: delete it from knownWithoutMockKeychain", name)
		}
	}
}

// The check itself can fail, and is not fooled by a comment.
func TestLauncherCheckFindsWhatItShould(t *testing.T) {
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	got, err := launchersWithoutMockKeychain(fstest.MapFS{
		"bare.sh":          file(`"$chrome" --headless=new --user-data-dir=/x` + "\n"),
		"port-only.ts":     file(`spawn(chrome, ["--remote-debugging-port=1"])` + "\n"),
		"comment-only.sh":  file("# --use-mock-keychain\n\"$chrome\" --headless=new\n"),
		"no-extension":     file("#!/bin/sh\n\"$chrome\" --headless=new\n"),
		"sub/nested.go":    file(`var a = []string{"--headless=new"}` + "\n"),
		"good.sh":          file(`"$chrome" --use-mock-keychain --headless=new` + "\n"),
		"good.go":          file(`var a = []string{"--use-mock-keychain", "--headless=new"}` + "\n"),
		"only-mentions.sh": file("# run with --headless\necho hi\n"),
		"notes.md":         file("--headless\n"),
		"x_test.go":        file(`var a = "--headless"` + "\n"),
		"testdata/a.sh":    file("--headless\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bare.sh", "comment-only.sh", "no-extension", "port-only.ts", "sub/nested.go"}
	if !slices.Equal(got, want) {
		t.Errorf("found %q, want %q", got, want)
	}
}
