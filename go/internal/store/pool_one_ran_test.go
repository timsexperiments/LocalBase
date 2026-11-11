//go:build !poolofone

package store_test

import (
	"slices"
	"strings"
	"testing"
)

// event is one line of a `go test -json` stream, in its real shape.
func event(action, pkg, test string) string {
	line := `{"Time":"2026-11-11T05:10:09Z","Action":"` + action + `","Package":"github.com/timsexperiments/LocalBase/go/` + pkg + `"`
	if test != "" {
		line += `,"Test":"` + test + `"`
	}
	return line + "}"
}

func stream(lines ...string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }

// TestNothingNeedsTwoConnections passes only when its runs ran what they name: a run that
// selects nothing ("no tests to run") passes too, and is how its gateway half once checked nothing.
func TestNotRunSaysWhatARunLeftOut(t *testing.T) {
	ranNothing := stream(
		event("start", "contract", ""),
		`{"Action":"output","Package":"github.com/timsexperiments/LocalBase/go/contract","Output":"testing: warning: no tests to run\n"}`,
		event("pass", "contract", ""))
	ranOne := stream(
		event("start", "test/system", ""),
		event("run", "test/system", "TestInstallCancel"),
		event("pass", "test/system", "TestInstallCancel/a_subtest"),
		event("pass", "test/system", "TestInstallCancel"),
		event("run", "test/system", "TestLimitedChangesCannotPushOutWhoChangedKeys"),
		event("fail", "test/system", "TestLimitedChangesCannotPushOutWhoChangedKeys"),
		event("pass", "test/system", ""))
	packages := stream(
		"# a line that isn't JSON, as a build failure would print",
		event("pass", "internal/auth", "TestA"),
		event("pass", "internal/auth", ""),
		event("pass", "internal/store", ""),
		event("pass", "internal/gateway/admin/keys", "TestB"),
		event("pass", "internal/gateway/admin/keys", ""))
	for _, tc := range []struct {
		name            string
		stream          []byte
		packages, tests []string
		want            []string
	}{
		{"named tests, none run", ranNothing, nil, []string{"TestInstallCancel"},
			[]string{"TestInstallCancel did not run and pass"}},
		{"named tests, all passed", ranOne, nil, []string{"TestInstallCancel"}, nil},
		{"a named test failed", ranOne, nil, []string{"TestInstallCancel", "TestLimitedChangesCannotPushOutWhoChangedKeys"},
			[]string{"TestLimitedChangesCannotPushOutWhoChangedKeys did not run and pass"}},
		{"a name misspelled", ranOne, nil, []string{"TestInstalCancel"}, []string{"TestInstalCancel did not run and pass"}},
		{"only a subtest of that name passed", ranOne, nil, []string{"a_subtest"}, []string{"a_subtest did not run and pass"}},
		{"packages, each with a test", packages, []string{"./internal/auth", "./internal/gateway/..."}, nil, nil},
		{"a package that ran none", packages, []string{"./internal/store"}, nil, []string{"no test passed in ./internal/store"}},
		{"a tree with none", packages, []string{"./internal/runtime/..."}, nil, []string{"no test passed in ./internal/runtime/..."}},
		// "./internal/gateway" alone is the gateway package, not the tree under it.
		{"a package, not its tree", packages, []string{"./internal/gateway"}, nil, []string{"no test passed in ./internal/gateway"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := notRun(tc.stream, tc.packages, tc.tests); !slices.Equal(got, tc.want) {
				t.Errorf("notRun = %q, want %q", got, tc.want)
			}
		})
	}
}

// A failure is shown as go test prints it, not as JSON.
func TestPlainOutputIsWhatGoTestPrints(t *testing.T) {
	got := plainOutput(stream(
		"# github.com/timsexperiments/LocalBase/go/contract",
		event("start", "contract", ""),
		`{"Action":"output","Package":"p","Output":"panic: test timed out after 4m0s\n"}`,
		`{"Action":"output","Package":"p","Output":"FAIL\tp\t240.1s\n"}`,
		event("fail", "contract", "")))
	if want := "# github.com/timsexperiments/LocalBase/go/contract\npanic: test timed out after 4m0s\nFAIL\tp\t240.1s\n"; got != want {
		t.Errorf("plainOutput = %q, want %q", got, want)
	}
}
