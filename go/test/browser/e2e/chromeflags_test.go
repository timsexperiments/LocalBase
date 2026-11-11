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

// A launch gives the port a value. A script that only looks for the flag in ps output doesn't.
var startsChrome = regexp.MustCompile(`--headless|--remote-debugging-port=|--remote-debugging-pipe`)

// launchersWithoutMockKeychain lists the scripts under fsys that start a Chrome without
// --use-mock-keychain, and counts the files it read. A flag only in a comment does not count.
func launchersWithoutMockKeychain(fsys fs.FS) (missing []string, read int, err error) {
	err = fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
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
		read++
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
	return missing, read, err
}

// launcherTrees are the trees whose scripts start Chrome, from this package: go/tools and go/test.
var launcherTrees = map[string]string{"tools": "../../../tools", "test": "../.."}

// minLaunchersRead is the fewest files the check reads across both trees. Fewer means a tree moved
// out of its reach, so the check fails rather than pass on less.
const minLaunchersRead = 100

// Every script under go/tools and go/test that starts Chrome keeps it away from the Keychain: see
// tools/README.md for what happens otherwise.
func TestEveryChromeLauncherStaysAwayFromTheKeychain(t *testing.T) {
	total := 0
	for prefix, dir := range launcherTrees {
		missing, read, err := launchersWithoutMockKeychain(os.DirFS(dir))
		if err != nil {
			t.Fatal(err)
		}
		total += read
		for _, name := range missing {
			t.Errorf("%s/%s starts Chrome without --use-mock-keychain --password-store=basic; see tools/README.md", prefix, name)
		}
	}
	if total < minLaunchersRead {
		t.Errorf("read %d files under go/tools and go/test, fewer than %d: a tree moved out of reach", total, minLaunchersRead)
	}
}

// The check itself can fail, and is not fooled by a comment.
func TestLauncherCheckFindsWhatItShould(t *testing.T) {
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	got, read, err := launchersWithoutMockKeychain(fstest.MapFS{
		"bare.sh":          file(`"$chrome" --headless=new --user-data-dir=/x` + "\n"),
		"port-only.ts":     file(`spawn(chrome, ["--remote-debugging-port=1"])` + "\n"),
		"comment-only.sh":  file("# --use-mock-keychain\n\"$chrome\" --headless=new\n"),
		"no-extension":     file("#!/bin/sh\n\"$chrome\" --headless=new\n"),
		"sub/nested.go":    file(`var a = []string{"--headless=new"}` + "\n"),
		"good.sh":          file(`"$chrome" --use-mock-keychain --headless=new` + "\n"),
		"good.go":          file(`var a = []string{"--use-mock-keychain", "--headless=new"}` + "\n"),
		"only-mentions.sh": file("# run with --headless\necho hi\n"),
		"only-looks.sh":    file("ps -o command= -p \"$p\" | grep -q -- --remote-debugging-port\n"),
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
	if read != 9 {
		t.Errorf("read %d files, want the 9 that are code", read)
	}
}
