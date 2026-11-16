package main

import (
	"slices"
	"testing"
)

// Without these, a current Chrome on macOS asks the Keychain for its cookie key: a prompt
// on the owner's screen, and navigation that may never return.
func TestChromeStaysAwayFromTheKeychain(t *testing.T) {
	for _, noSandbox := range []bool{false, true} {
		args := chromeArgs(29000, "/scratch/profile", noSandbox)
		for _, want := range []string{"--use-mock-keychain", "--password-store=basic", "--user-data-dir=/scratch/profile", "--headless=new"} {
			if !slices.Contains(args, want) {
				t.Errorf("no %s in %q", want, args)
			}
		}
		if got := slices.Contains(args, "--no-sandbox"); got != noSandbox {
			t.Errorf("--no-sandbox is %v, want %v, in %q", got, noSandbox, args)
		}
		if args[len(args)-1] != "about:blank" {
			t.Errorf("the page must come last: %q", args)
		}
	}
}
