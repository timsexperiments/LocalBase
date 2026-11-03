package contract

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestDifferentialRetrySemanticsFake is TestDifferentialRetrySemantics without a real sd-server, so
// that it runs in CI: a fake that refuses to start while a file exists, and then starts. A failed
// start is transient on both gateways: 503 with Retry-After on each repeat, with the same envelope.
// Once the fault is gone, Go's next request works, because it retries the start.
func TestDifferentialRetrySemanticsFake(t *testing.T) {
	impls := []string{*implFlag}
	if *implFlag == "both" {
		requireTS(t)
		impls = []string{"ts", "go"}
	}
	if *urlFlag != "" {
		t.Skip("launches its own gateways")
	}
	tmp := scratchDir(t)
	fake := filepath.Join(tmp, "fakebackend")
	if out, err := exec.Command("go", "build", "-o", fake, "../internal/runtime/fakebackend").CombinedOutput(); err != nil {
		t.Fatalf("build fakebackend: %v %s", err, out)
	}
	type answer struct {
		status      int
		code        string
		retry       bool
		message     string
		afterTheFix int
	}
	answers := map[string]answer{}
	for _, impl := range impls {
		bin := filepath.Join(tmp, impl+"-bin")
		if err := mustMkdirAll(bin, 0o700); err != nil {
			t.Fatal(err)
		}
		broken := filepath.Join(tmp, impl+"-broken")
		if err := mustWriteFile(broken, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		script := filepath.Join(bin, "sd-server")
		if err := mustWriteFile(script, []byte(fmt.Sprintf("#!/bin/sh\n[ -e %q ] && exit 3\nexec %q \"$@\"\n", broken, fake)), 0o700); err != nil {
			t.Fatal(err)
		}
		// Go is told where it is; TS finds a user-managed one on PATH.
		tg, err := launchEnv(impl, []string{"LOCALBASE_SD_SERVER=" + script, "PATH=" + bin + ":" + os.Getenv("PATH")})
		if err != nil {
			t.Fatal(err)
		}
		defer tg.stop()
		ask := func() *Resp {
			h := bearer(tg.Key)
			h["Content-Type"] = "application/json"
			return do(t, "POST", tg.BaseURL+"/v1/images/generations", h,
				`{"model":"dreamshaper-8-lcm","prompt":"a","size":"256x256","response_format":"b64_json"}`)
		}
		var a answer
		for i := 1; i <= 2; i++ {
			r := ask()
			e, _ := r.JSON(t)["error"].(map[string]any)
			if e["code"] == "insufficient_memory" {
				t.Skipf("%s: refused for memory on this machine, so the start wasn't reached", impl)
			}
			a = answer{status: r.Status, retry: r.Header.Get("Retry-After") != ""}
			a.code, _ = e["code"].(string)
			a.message, _ = e["message"].(string)
			if a.status != http.StatusServiceUnavailable || a.code != "service_unavailable" || !a.retry {
				t.Errorf("%s, failed start #%d: %d %s Retry-After=%v: %s", impl, i, a.status, a.code, a.retry, r.Body)
			}
		}
		if err := mustRemoveAll(broken); err != nil {
			t.Fatal(err)
		}
		a.afterTheFix = ask().Status
		answers[impl] = a
	}
	if a, ok := answers["go"]; ok && a.afterTheFix != http.StatusOK {
		t.Errorf("go after the fault was fixed: %d, want a retried start", a.afterTheFix)
	}
	if ts, ok := answers["ts"]; ok {
		t.Logf("ts after the fault was fixed: %d", ts.afterTheFix)
		g := answers["go"]
		if ts.status != g.status || ts.code != g.code || ts.retry != g.retry || ts.message != g.message {
			t.Errorf("failed starts differ:\n ts %+v\n go %+v", ts, g)
		}
	}
}
