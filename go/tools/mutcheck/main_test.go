package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	source = "package demo\n\nfunc Add(a, b int) int { return a + b }\n\nfunc Unused() int { return 1 }\n"
	tests  = "package demo\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n"
)

// demoModule is a small module whose test passes, to break on purpose.
func demoModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"go.mod": "module demo\n\ngo 1.21\n", "demo.go": source, "demo_test.go": tests} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	return dir
}

func plenty(string) (uint64, error) { return 1000, nil }

func TestResults(t *testing.T) {
	dir := demoModule(t)
	muts := []Mutation{
		{Name: "noticed", File: "demo.go", From: "a + b", To: "a - b", Pkg: "./", Run: "TestAdd"},
		{Name: "not noticed", File: "demo.go", From: "return 1", To: "return 2", Pkg: "./", Run: "TestAdd"},
		{Name: "a compile error", File: "demo.go", From: "a + b", To: "a + undefinedName", Pkg: "./", Run: "TestAdd"},
		{Name: "breaks the test's own build", File: "demo.go", From: "func Add(a, b int) int", To: "func Add(a int) int", Pkg: "./", Run: "TestAdd"},
		{Name: "nothing to change", File: "demo.go", From: "not in the file", To: "x", Pkg: "./", Run: "TestAdd"},
	}
	var out bytes.Buffer
	results, err := Check(context.Background(), muts, limits{timeout: time.Minute, minFreeGB: 90, dir: dir, freeGB: plenty}, &out)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"noticed": Killed, "not noticed": Survived, "a compile error": DidNotCompile,
		"breaks the test's own build": DidNotCompile, "nothing to change": NoMatch}
	for _, r := range results {
		if r.Outcome != want[r.Name] {
			t.Errorf("%s: %s (%s), want %s", r.Name, r.Outcome, r.Detail, want[r.Name])
		}
		if !strings.Contains(out.String(), r.Outcome) {
			t.Errorf("not printed: %s\n%s", r.Outcome, out.String())
		}
	}
	if len(results) != len(muts) {
		t.Errorf("%d results for %d mutations", len(results), len(muts))
	}
	if b, _ := os.ReadFile("demo.go"); string(b) != source {
		t.Errorf("the file was not put back: %s", b)
	}
}

// A test that already fails says nothing about a mutation.
func TestRefusesWhenTheTestsAlreadyFail(t *testing.T) {
	dir := demoModule(t)
	if err := os.WriteFile("demo.go", []byte(strings.Replace(source, "a + b", "a - b", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	muts := []Mutation{{Name: "m", File: "demo.go", From: "return 1", To: "return 2", Pkg: "./", Run: "TestAdd"}}
	results, err := Check(context.Background(), muts, limits{timeout: time.Minute, minFreeGB: 90, dir: dir, freeGB: plenty}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "have to pass") || len(results) != 0 {
		t.Errorf("%v %v", results, err)
	}
}

func TestStopsAtTheLimits(t *testing.T) {
	dir := demoModule(t)
	if err := os.WriteFile("demo_test.go", []byte(`package demo

import (
	"testing"
	"time"
)

func TestAdd(t *testing.T) {
	if Add(1, 2) != 3 {
		time.Sleep(time.Hour)
	}
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	muts := []Mutation{{Name: "hangs", File: "demo.go", From: "a + b", To: "a - b", Pkg: "./", Run: "TestAdd"}}
	var out bytes.Buffer
	results, err := Check(context.Background(), muts, limits{timeout: 10 * time.Second, minFreeGB: 90, dir: dir, freeGB: plenty}, &out)
	if err != nil || len(results) != 1 || results[0].Outcome != Stopped {
		t.Errorf("time limit: %+v %v", results, err)
	}

	// The disk goes under the floor partway through a mutation's run.
	calls := 0
	lowLater := func(string) (uint64, error) {
		calls++
		if calls <= 2 {
			return 1000, nil
		}
		return 1, nil
	}
	results, err = Check(context.Background(), muts, limits{timeout: time.Minute, minFreeGB: 90, dir: dir, freeGB: lowLater}, &out)
	if err != nil || len(results) != 1 || results[0].Outcome != Stopped || !strings.Contains(results[0].Detail, "floor") {
		t.Errorf("floor: %+v %v", results, err)
	}

	_, err = Check(context.Background(), muts, limits{timeout: time.Minute, minFreeGB: 90, dir: dir, freeGB: func(string) (uint64, error) { return 1, nil }}, &out)
	if err == nil || !strings.Contains(err.Error(), "floor") {
		t.Errorf("under the floor from the start: %v", err)
	}
	if b, _ := os.ReadFile("demo.go"); string(b) != source {
		t.Errorf("the file was not put back: %s", b)
	}
}

// The command itself, as a person runs it: the exit status says whether all were killed.
func TestCommandExitStatus(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mutcheck")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	demoModule(t)
	run := func(spec string) (int, string) {
		t.Helper()
		if err := os.WriteFile("spec.json", []byte(spec), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, "-min-free-gb", "0", "spec.json")
		out, err := cmd.CombinedOutput()
		if err != nil && cmd.ProcessState == nil {
			t.Fatal(err)
		}
		return cmd.ProcessState.ExitCode(), string(out)
	}
	if code, out := run(`[{"name":"k","file":"demo.go","from":"a + b","to":"a - b","pkg":"./","run":"TestAdd"}]`); code != 0 || !strings.Contains(out, "killed") {
		t.Errorf("all killed: %d %s", code, out)
	}
	code, out := run(`[{"name":"c","file":"demo.go","from":"a + b","to":"a + nope","pkg":"./","run":"TestAdd"}]`)
	if code != 1 || !strings.Contains(out, "did not compile") || strings.Contains(out, "survived") {
		t.Errorf("a compile error: %d %s", code, out)
	}
}
