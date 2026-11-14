package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The helper renders each golden case's unit from a manifest shaped like the one the binary writes.
func TestRendersTheGoldenUnits(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "service", "testdata", "units.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Hash  string `json:"hash"`
		Cases []struct {
			Name    string            `json:"name"`
			Program string            `json:"program"`
			Args    []string          `json:"args"`
			Root    string            `json:"root"`
			Otel    map[string]string `json:"otel"`
			Token   string            `json:"token"`
			Systemd string            `json:"systemd"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "unitrender")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	n := 0
	for _, c := range g.Cases {
		if len(c.Otel) > 0 { // the installed unit carries no OTEL_* settings in the check
			continue
		}
		manifest, _ := json.Marshal(map[string]any{
			"version": 1, "root": c.Root, "manager": "systemd-user",
			"serviceId":      "com.localbase.gateway." + g.Hash,
			"definitionPath": "/home/lb/.config/systemd/user/local-base-gateway-" + g.Hash + ".service",
			"serviceToken":   c.Token,
			"invocation":     map[string]any{"program": c.Program, "arguments": c.Args, "serviceToken": c.Token},
		})
		cmd := exec.Command(bin)
		cmd.Stdin = strings.NewReader(string(manifest))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if string(out) != c.Systemd {
			t.Errorf("%s:\n%s\nwant\n%s", c.Name, out, c.Systemd)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no golden case without OTEL settings")
	}
}
