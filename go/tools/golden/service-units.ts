// Golden service definitions from TS (domains/service/definitions.ts): the
// systemd user unit and the launchd plist for awkward paths and arguments
// (spaces, $, %, quotes, backslashes, specifiers, non-ASCII) and telemetry.
//
//   LOCALBASE_ROOT=/nonexistent bun run go/tools/golden/service-units.ts \
//     > go/internal/service/testdata/units.json
import {
  renderLaunchdDefinition,
  renderSystemdDefinition,
} from "../../../src/domains/service/definitions";

const hash = "a".repeat(64);
const cases = [
  { name: "plain", program: "/usr/local/bin/local-base", args: ["--root", "/lb-golden/u/.local/share/local-base", "serve"], root: "/lb-golden/u/.local/share/local-base", otel: {} },
  { name: "spaces", program: "/opt/local base/lb", args: ["--root", "/r 1", "serve"], root: "/r 1", otel: {} },
  { name: "specials", program: "/opt/a$b%c/lb", args: ["--root", "/lb-golden/q\"uo'te\\back", "serve", "--port", "2273"], root: "/lb-golden/q\"uo'te\\back", otel: {} },
  { name: "specifiers", program: "/opt/%h/%n/lb", args: ["--root", "/data/%i;x", "serve"], root: "/data/%i;x", otel: {} },
  { name: "unicode", program: "/Users/zoë/bin/lb", args: ["--root", "/lb-golden/zoë/Local Base", "serve"], root: "/lb-golden/zoë/Local Base", otel: {} },
  { name: "telemetry", program: "/usr/bin/lb", args: ["--root", "/r", "serve"], root: "/r",
    otel: { OTEL_TRACES_SAMPLER: "always_on", OTEL_EXPORTER_OTLP_ENDPOINT: "http://127.0.0.1:4318", OTEL_EXPORTER_OTLP_HEADERS: "authorization=Bearer a b" } },
];
const token = "6f1c2c3e-8a7b-4c1d-9e2f-0123456789ab";
const out = cases.map((c) => {
  const metadata = (platform: "linux" | "darwin") => ({
    platform,
    manager: platform === "linux" ? "systemd-user" : "launchd",
    serviceId: `com.localbase.gateway.${hash}`,
    unitName: `local-base-gateway-${hash}.service`,
    definitionPath: platform === "linux" ? `/home/u/.config/systemd/user/local-base-gateway-${hash}.service` : `/Users/u/Library/LaunchAgents/com.localbase.gateway.${hash}.plist`,
  }) as const;
  const invocation = { program: c.program, arguments: c.args };
  return {
    ...c,
    token,
    systemd: renderSystemdDefinition(metadata("linux"), invocation, c.root, token, c.otel),
    launchd: renderLaunchdDefinition(metadata("darwin"), invocation, c.root, token, c.otel),
  };
});
console.log(JSON.stringify({ hash, cases: out }, null, 2));
