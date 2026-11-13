// Package system holds the black-box tests that launch a LocalBase gateway of their own, run its
// command line or run its built binaries (step 7, R3). go/test/contract keeps the tests that run only
// against the shared targets. Both take the same flags and environment through go/test/harness;
// go/test/contract's package doc lists them, and tools/contract.sh runs both packages.
package system

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

// TestMain runs the package's tests through the harness, which launches the shared targets
// when a test first asks and stops them at the end, then removes the CLI builds and the upgrade
// kit's binaries.
func TestMain(m *testing.M) {
	code := harness.Main(m)
	removeGoCLI()
	removeUpgradeKit()
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
