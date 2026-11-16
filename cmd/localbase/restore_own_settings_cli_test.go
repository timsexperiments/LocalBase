package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/backup"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
)

// A real backup and the real command: settings changed with their own commands show in the report by
// what they do, set against the root being restored into, with the caution last before the question.
func TestARestoreReportSaysWhatTheRootDoesByItself(t *testing.T) {
	root := noAuthRoot(t)
	for _, args := range [][]string{{"upgrade", "check-updates", "on"}, {"speech", "marking", "off"}} {
		if code, out := runApp(t, append([]string{"--root", root}, args...)...); code != 0 {
			t.Fatalf("%v: %s", args, out)
		}
	}
	file := filepath.Join(t.TempDir(), "b.lbbackup")
	if code, out := runApp(t, "--root", root, "backup", "--output", file); code != 0 {
		t.Fatalf("backup: %s", out)
	}
	code, out := runApp(t, "--root", filepath.Join(t.TempDir(), "target"), "restore", "--dry-run", "--no-port-probe", file)
	if code != 0 {
		t.Fatalf("restore: %s", out)
	}
	for _, want := range []string{
		"What this root does by itself:\n" +
			"  Checks GitHub once a day for a newer version: on (a new root has it off)\n" +
			"  Marks synthesized speech as made by AI: off (a new root has it on)\n",
		`This turns off marking synthesized speech as made by AI. Telling people it is becomes your responsibility. See "What this root does by itself" above.`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	code, asJSON := runApp(t, "--json", "--root", filepath.Join(t.TempDir(), "target"), "restore", "--dry-run", "--no-port-probe", file)
	if code != 0 {
		t.Fatalf("restore --json: %s", asJSON)
	}
	var envelope struct {
		Data struct {
			Report struct {
				OwnSettings []map[string]any `json:"ownSettings"`
			} `json:"report"`
		} `json:"data"`
	}
	// The JSON is the first line. What follows went to stderr, as it does for a person at a terminal.
	if err := json.Unmarshal([]byte(asJSON[:strings.Index(asJSON, "\n")]), &envelope); err != nil {
		t.Fatalf("%v: %.400s", err, asJSON)
	}
	want := []map[string]any{{"setting": "update.check", "value": "on"}, {"setting": "speech.provenance-marking", "value": "off"}}
	if got, _ := json.Marshal(envelope.Data.Report.OwnSettings); !bytes.Equal(got, mustJSON(t, want)) {
		t.Errorf("ownSettings: %s", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The heading, and a line for each setting, in the order the report holds them.
func TestTheOwnSettingsBlockReads(t *testing.T) {
	var out bytes.Buffer
	printOwnSettings(&cli.Ctx{Stdout: &out, Stderr: &out}, &backup.Report{OwnSettings: []backup.OwnSetting{
		{Setting: "update.check", Value: "off", ThisRoot: "off"},
		{Setting: "speech.provenance-marking", Value: "off", ThisRoot: "on"},
	}})
	want := "What this root does by itself:\n" +
		"  Checks GitHub once a day for a newer version: off\n" +
		"  Marks synthesized speech as made by AI: off (this root has it on)\n"
	if out.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}
