package service

import (
	"encoding/json"
	"os"
	"testing"
)

// TestUnitsMatchTS renders the systemd user unit and the launchd plist for
// awkward paths, arguments and telemetry, byte for byte as TS does
// (tools/golden/service-units.ts).
func TestUnitsMatchTS(t *testing.T) {
	raw, err := os.ReadFile("testdata/units.json")
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
			Launchd string            `json:"launchd"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range g.Cases {
		inv := Invocation{Program: c.Program, Arguments: c.Args}
		linux := Metadata{Platform: "linux", Manager: "systemd-user", ServiceID: "com.localbase.gateway." + g.Hash,
			UnitName: "local-base-gateway-" + g.Hash + ".service"}
		if got := RenderSystemd(linux, inv, c.Root, c.Token, c.Otel); got != c.Systemd {
			t.Errorf("%s systemd:\n%s\nwant\n%s", c.Name, got, c.Systemd)
		}
		mac := Metadata{Platform: "darwin", Manager: "launchd", ServiceID: "com.localbase.gateway." + g.Hash,
			UnitName: "local-base-gateway-" + g.Hash + ".service"}
		if got := RenderLaunchd(mac, inv, c.Root, c.Token, c.Otel); got != c.Launchd {
			t.Errorf("%s launchd:\n%s\nwant\n%s", c.Name, got, c.Launchd)
		}
	}
}
