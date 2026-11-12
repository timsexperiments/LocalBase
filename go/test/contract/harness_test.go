// Package contract is a black-box HTTP contract suite that runs against the
// TypeScript and the Go gateway alike.
//
//	go test ./test/contract -args -impl=go          # build and launch the Go gateway
//	go test ./test/contract -args -impl=ts          # launch `bun run src/cli.ts serve`
//	go test ./test/contract -args -impl=both        # both, plus differential checks
//	go test ./test/contract -args -url=http://127.0.0.1:P -key=lb_... -keyless-key=lb_...
//	                                                # an already running gateway
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
	"os"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness"
)

// TestMain runs the package's tests through the harness, which launches the shared targets
// when a test first asks and stops them at the end. The CLI builds went with go/test/system.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m))
}

// each runs f as a subtest per shared target. Only this package's tests use it, so it lives here
// rather than in the harness (step 7's rule for exports, R3).
func each(t *testing.T, f func(t *testing.T, tg *harness.Target)) {
	for _, tg := range harness.Targets(t) {
		t.Run(tg.Name, func(t *testing.T) { f(t, tg) })
	}
}
