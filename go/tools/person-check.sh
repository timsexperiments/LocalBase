#!/bin/sh
# Two people on one browser profile: whose chats, drafts, titles and pages each of them sees, signed
# in for real through a fake OIDC provider over TLS. Builds a scratch gateway with a fake model and
# generates its own certificate; nothing here touches a real root or a real account.
#   tools/person-check.sh [first port]   # uses 5 ports from there, default 24650
# If a run dies, its gateway follows within a few seconds (supervise.sh).
# LBGO_CHROME picks the browser. Check that every step ran:
#   tools/person-check.sh > log; bun tools/check-manifest/run.ts check tools/person-check/run.ts log
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
first=${1:-${PERSON_CHECK_PORT:-24650}}
chrome=${LBGO_CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
work="$here/.cache/person-check"
profile="$here/.cache/browser-profile-person-check"
. "$here/tools/browser-run.sh"
browser_guard person-check
(cd "$here" && rm -rf .cache/browser-profile-person-check .cache/person-check)
mkdir -p "$work"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -keyout "$work/key.pem" -out "$work/cert.pem" -subj /CN=localhost -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" >/dev/null 2>&1
cat > "$work/zz_person_check_tls.go" <<GO
package auth

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
)

// Only in this tool's own build, through -overlay: trust the fake provider's certificate.
func init() {
	pem, err := os.ReadFile("$work/cert.pem")
	if err != nil {
		return
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool}
}
GO
echo "{\"Replace\":{\"$here/internal/auth/zz_person_check_tls.go\":\"$work/zz_person_check_tls.go\"}}" > "$work/overlay.json"
(cd "$here" && go build -overlay "$work/overlay.json" -o "$work/local-base" ./cmd/localbase && go build -o "$work/llama-server" ./internal/runtime/fakebackend)
"$work/local-base" --root "$work/root" --non-interactive init >"$work/init.log" 2>&1
mkdir -p "$work/root/models/llm"
mkfile -n 3013027808 "$work/root/models/llm/Qwen_Qwen3.5-4B-Q4_K_M.gguf" 2>/dev/null || truncate -s 3013027808 "$work/root/models/llm/Qwen_Qwen3.5-4B-Q4_K_M.gguf"
sqlite3 "$work/root/local-base.db" "update config set memory_system_reserve_percent=0, memory_system_reserve_minimum_gb=0, memory_accelerator_reserve_percent=0"
cat > "$work/root/ui-access.json" <<JSON
{"provider":{"kind":"direct","registrations":[{"kind":"oidc","id":"idp","name":"Test Provider","issuer":"https://localhost:$first","clientId":"cid","clientAuthentication":{"kind":"none"}}]},"origin":"https://localhost:$((first + 1))","permissions":["inference:chat","models:read"]}
JSON
cat > "$work/policy.json" <<JSON
{"roles":[{"name":"member","description":"Member","permissions":["inference:chat","models:read"]},{"name":"owner","description":"Owner","permissions":["inference:chat","models:read","access:read","access:manage"]}],"bindings":[{"kind":"email","role":"owner","email":"carol@example.com"},{"kind":"email","role":"member","email":"alice@example.com"},{"kind":"email","role":"member","email":"bob@example.com"}]}
JSON
"$work/local-base" --root "$work/root" --non-interactive access policy apply --file "$work/policy.json" >"$work/policy.log" 2>&1
"$work/local-base" --root "$work/root" --non-interactive access users invite --email=alice@example.com --roles=member >"$work/invite.log" 2>&1
cdp=$((first + 4))
browser_group "$chrome" --headless=new --use-mock-keychain --password-store=basic --disable-gpu --no-first-run --ignore-certificate-errors --remote-debugging-address=127.0.0.1 --remote-debugging-port=$cdp --user-data-dir="$profile" about:blank >/dev/null 2>&1 &
browser_started $!
for i in $(seq 1 30); do curl -s "http://127.0.0.1:$cdp/json/version" >/dev/null && break; sleep 0.5; done
browser_run bun run "$here/tools/person-check/run.ts" "http://127.0.0.1:$cdp" "$work" "$first"
