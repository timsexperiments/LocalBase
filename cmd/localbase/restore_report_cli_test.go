package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const backendWithEscapes = `{"schema":"localbase.backend/v1","id":"custom","name":"Custom","engine":"fake-engine","adapter":"process-openai/v1","tasks":["chat"],"formats":["gguf"],"architectures":{"supported":["x"]},"platforms":[{"os":"darwin","arch":"arm64","accelerator":"cpu"},{"os":"linux","arch":"amd64","accelerator":"cpu"},{"os":"linux","arch":"arm64","accelerator":"cpu"}],"capabilities":[],"launch":{"command":"/bin/sh","args":["-c","run","--port","{port}","\u001b[1A\u001b[2Kplanted"],"healthPath":"/health","memory":{"assumption":"none","weightsFactor":1,"overheadGb":0}}}`

// backupWithEscapes makes a real backup whose one custom backend has an argument that moves the
// cursor and erases a line, and returns the file.
func backupWithEscapes(t *testing.T) string {
	t.Helper()
	root := noAuthRoot(t)
	p := filepath.Join(root, "backends.d", "custom.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(backendWithEscapes), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "b.lbbackup")
	if code, out := runApp(t, "--root", root, "backup", "--output", file); code != 0 {
		t.Fatalf("backup: %s", out)
	}
	return file
}

// The report is what a person reads before saying yes, so what's in a file can't move the cursor
// and erase the line saying what would run as them.
func TestARestoreReportShowsControlCharactersAsEscapes(t *testing.T) {
	file := backupWithEscapes(t)
	code, out := runApp(t, "--root", filepath.Join(t.TempDir(), "target"), "restore", "--dry-run", "--no-port-probe", file)
	if code != 0 {
		t.Fatalf("restore: %s", out)
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("the report holds a raw escape: %q", out)
	}
	if !strings.Contains(out, `\x1b[1A\x1b[2Kplanted`) {
		t.Errorf("the report doesn't show it as an escape: %q", out)
	}
}

// --json carries what the file says, unchanged: a program reading it gets the real value.
func TestARestoreReportInJSONKeepsTheValue(t *testing.T) {
	file := backupWithEscapes(t)
	code, out := runApp(t, "--json", "--root", filepath.Join(t.TempDir(), "target"), "restore", "--dry-run", "--no-port-probe", file)
	if code != 0 {
		t.Fatalf("restore: %s", out)
	}
	line := out[:strings.Index(out, "\n")]
	var env struct {
		Data struct {
			Report struct {
				Programs []struct {
					Command []string `json:"command"`
				} `json:"programs"`
			} `json:"report"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	if p := env.Data.Report.Programs; len(p) != 1 || p[0].Command[len(p[0].Command)-1] != "\x1b[1A\x1b[2Kplanted" {
		t.Errorf("programs: %q", p)
	}
}
