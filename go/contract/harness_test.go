// Package contract is a black-box HTTP contract suite that runs against the
// TypeScript and the Go gateway alike.
//
//	go test ./contract -args -impl=go          # build and launch the Go gateway
//	go test ./contract -args -impl=ts          # launch `bun run src/cli.ts serve`
//	go test ./contract -args -impl=both        # both, plus differential checks
//	go test ./contract -args -url=http://127.0.0.1:P -key=lb_... -keyless-key=lb_...
//	                                           # an already running gateway
//
// Launched gateways get a fresh scratch root in a temp dir and ports from
// LOCALBASE_TEST_PORTS (default 25000-25499). LOCALBASE_CONTRACT_MODELS names a models/llm directory (read
// only) holding qwen2.5-coder-1.5b-instruct-q4_k_m; the TS gateway needs it
// because it refuses to start without the active model's files. Adding
// LOCALBASE_CONTRACT_RUNTIMES (a bin/runtimes directory, copied) runs the
// real-inference cases.
//
// Image admission tests need free memory: do not run them alongside other
// model loads, or memory refusals can change which error a gateway reaches.
//
// LOCALBASE_CONTRACT_COVERDIR builds the Go binaries with -cover and collects
// their coverage there, which is the only coverage of what this suite exercises
// (go/docs/production-inventory.md).
package contract

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
	"github.com/timsexperiments/LocalBase/go/test/harness"
)

// TestMain runs the package's tests through the harness, which launches the shared harness.Targets(t)
// when a test first asks and stops them at the end, then removes the CLI builds.
func TestMain(m *testing.M) {
	code := harness.Main(m)
	removeGoCLI()
	os.Exit(code)
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// rangeServer is httptest.NewServer (or NewTLSServer) listening on a port in
// the test block (testutil.FreePort) instead of an ephemeral one.
func rangeServer(t testing.TB, h http.Handler, tls bool) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(h)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", testutil.FreePort()))
	if err != nil {
		t.Fatal(err)
	}
	ts.Listener.Close()
	ts.Listener = ln
	if tls {
		ts.StartTLS()
	} else {
		ts.Start()
	}
	t.Cleanup(ts.Close)
	return ts
}
