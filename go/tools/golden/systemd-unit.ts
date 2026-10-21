// Renders the systemd user unit TS would install for a service manifest
// (<root>/service/manifest.json on stdin), for tools/systemd-check.sh to
// compare byte for byte with the unit the Go binary installed.
import { basename } from "node:path";
import { renderSystemdDefinition } from "../../../src/domains/service/definitions";

const manifest = JSON.parse(await Bun.stdin.text());
const { serviceToken: _token, ...invocation } = manifest.invocation;
process.stdout.write(
  renderSystemdDefinition(
    {
      platform: "linux",
      manager: "systemd-user",
      serviceId: manifest.serviceId,
      unitName: basename(manifest.definitionPath),
      definitionPath: manifest.definitionPath,
    },
    invocation,
    manifest.root,
    manifest.serviceToken,
  ),
);
