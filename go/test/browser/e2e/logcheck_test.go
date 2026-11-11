package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// noPartOf fails if any run of six characters of the key is in what the check said.
func noPartOf(t *testing.T, said, key string) {
	t.Helper()
	body := strings.TrimPrefix(key, "lb_")
	for i := 0; i+6 <= len(body); i++ {
		if strings.Contains(said, body[i:i+6]) {
			t.Errorf("part of the key is in %q", said)
			return
		}
	}
}

func TestALoggedKeyIsNamedByLineAndNeverPrinted(t *testing.T) {
	const ordinary = `{"eventName":"http.request","message":"ok"}`
	issued := "lb_" + strings.Repeat("Ab3_", 8)
	short := "not-key-shaped-but-this-runs-own-key"
	for _, c := range []struct {
		name, line string
		secrets    []string
		key        string
	}{
		{"a key of this run", `{"auth":"` + issued + `"}`, []string{issued}, issued},
		{"key-shaped, and not this run's", `{"header":"Bearer ` + plantKey() + `"}`, nil, plantKey()},
		{"known to this run, though not key-shaped", `{"x":"` + short + `"}`, []string{short}, short},
		{"in a panic, which a key outranks", `panic: bad request from ` + plantKey(), nil, plantKey()},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := writeLog(t, ordinary, ordinary, c.line, ordinary)
			said := checkLog(path, c.secrets)
			if !strings.Contains(said, "an API key is in the log") || !strings.Contains(said, path+" line 3") {
				t.Errorf("does not name the file and line: %q", said)
			}
			noPartOf(t, said, c.key)
		})
	}
}

func TestAKeyAfterAPanicIsStillFound(t *testing.T) {
	path := writeLog(t, "panic: runtime error", `{"auth":"`+plantKey()+`"}`)
	if said := checkLog(path, nil); !strings.Contains(said, path+" line 2") {
		t.Errorf("the key on line 2 was not what it reported: %q", said)
	}
}

func TestPanicsAreReported(t *testing.T) {
	for _, c := range []struct{ name, line, want, not string }{
		{"a process panic", "panic: runtime error: index out of range", "runtime error", ""},
		{"a fatal error", "fatal error: concurrent map writes", "concurrent map", ""},
		{"a handler panic", `{"eventName":"http.panic","message":"nil map"}`, "nil map", ""},
		{"a panic quoting part of a key, whose words are left out", `{"eventName":"http.panic","message":"token lb_Zq9x was bad"}`, "line 1", "Zq9x"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := writeLog(t, c.line)
			said := checkLog(path, nil)
			if !strings.Contains(said, "panicked") || !strings.Contains(said, path+" line 1") || !strings.Contains(said, c.want) {
				t.Errorf("got %q", said)
			}
			if c.not != "" && strings.Contains(said, c.not) {
				t.Errorf("printed %q: %q", c.not, said)
			}
		})
	}
}

func TestACleanLogSaysNothing(t *testing.T) {
	path := writeLog(t, `{"eventName":"http.request","message":"key prefix lb_abcd1234"}`, "plain text")
	if said := checkLog(path, []string{""}); said != "" {
		t.Errorf("got %q", said)
	}
}

func TestAMissingLogIsAProblem(t *testing.T) {
	if checkLog(filepath.Join(t.TempDir(), "absent.log"), nil) == "" {
		t.Error("a log that is not there passed")
	}
}

func TestResultSaysTheLogFailed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.md")
	if err := os.WriteFile(path, []byte("# Browser suite: PASS\n\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	noteInResult(dir, "an API key is in the log: x line 3")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.HasPrefix(s, "# Browser suite: GATEWAY LOG FAILED") || !strings.Contains(s, "x line 3") {
		t.Errorf("got %q", s)
	}
}
