// Command unitrender prints the systemd user unit that service.RenderSystemd makes for a service
// manifest (<root>/service/manifest.json on stdin). systemd-check.sh compares it byte for byte with
// the unit the binary installed. The renderer itself is pinned by service.TestUnitsMatchTS, whose
// goldens were recorded from the TS renderer.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/timsexperiments/LocalBase/go/internal/service"
)

func main() {
	var m service.Manifest
	if err := json.NewDecoder(os.Stdin).Decode(&m); err != nil {
		fmt.Fprintln(os.Stderr, "unitrender: manifest:", err)
		os.Exit(1)
	}
	md := service.Metadata{Platform: "linux", Manager: "systemd-user", ServiceID: m.ServiceID,
		UnitName: filepath.Base(m.DefinitionPath), DefinitionPath: m.DefinitionPath}
	inv := m.Invocation
	inv.ServiceToken = ""
	fmt.Print(service.RenderSystemd(md, inv, m.Root, m.ServiceToken, map[string]string{}))
}
