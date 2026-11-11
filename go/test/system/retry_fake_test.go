package system

import (
	"flag"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// TestDifferentialRetrySemanticsFake is TestDifferentialRetrySemantics without a real backend, so that
// it runs in CI: a fake whisper-server that refuses to start while a file exists, and then starts. It
// is transcription, not images, because a CI runner has too little free memory to admit an image
// model's estimate even with no reserve, and transcription goes through the same failed-start path. A failed
// start is transient on both gateways: 503 with Retry-After on each repeat, with the same envelope.
// Once the fault is gone, Go's next request works, because it retries the start. The reserve is off,
// so that this reaches the start on a small CI runner too, and a refusal for memory fails the test.
func TestDifferentialRetrySemanticsFake(t *testing.T) {
	impls := []string{harness.Impl()}
	if harness.Impl() == "both" {
		harness.RequireTS(t)
		impls = []string{"ts", "go"}
	}
	if flag.Lookup("url").Value.String() != "" {
		t.Skip("launches its own gateways")
	}
	// Whisper's backends need Metal or Vulkan. Without one, a request stops at backend_not_selected
	// and never reaches the start this is about, as on a Linux CI runner.
	if goruntime.GOOS != "darwin" || goruntime.GOARCH != "arm64" {
		t.Skip("needs a whisper backend this host can select: Apple Silicon here")
	}
	tmp := fsguard.ScratchDir(t)
	fake := filepath.Join(tmp, "fakebackend")
	if out, err := exec.Command("go", "build", "-o", fake, "github.com/timsexperiments/LocalBase/go/internal/runtime/fakebackend").CombinedOutput(); err != nil {
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
		if err := fsguard.MkdirAll(bin, 0o700); err != nil {
			t.Fatal(err)
		}
		broken := filepath.Join(tmp, impl+"-broken")
		if err := fsguard.WriteFile(broken, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		script := filepath.Join(bin, "whisper-server")
		if err := fsguard.WriteFile(script, []byte(fmt.Sprintf("#!/bin/sh\n[ -e %q ] && exit 3\nexec %q \"$@\"\n", broken, fake)), 0o700); err != nil {
			t.Fatal(err)
		}
		// Go is told where it is; TS finds a user-managed one on PATH.
		tg, err := harness.Launch(impl, harness.Env([]string{"LOCALBASE_WHISPER_SERVER=" + script, "PATH=" + bin + ":" + os.Getenv("PATH")}), harness.NoReserve())
		if err != nil {
			t.Fatal(err)
		}
		defer tg.Stop()
		ask := func() *harness.Resp {
			var b strings.Builder
			mw := multipart.NewWriter(&b)
			_ = mw.WriteField("model", "whisper-base-en-q8_0")
			fw, err := mw.CreateFormFile("file", "a.wav")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = fw.Write([]byte("RIFF\x24\x00\x00\x00WAVEfmt "))
			_ = mw.Close()
			h := harness.Bearer(tg.Key)
			h["Content-Type"] = mw.FormDataContentType()
			return harness.Do(t, "POST", tg.BaseURL+"/v1/audio/transcriptions", h, b.String())
		}
		var a answer
		for i := 1; i <= 2; i++ {
			r := ask()
			e, _ := r.JSON(t)["error"].(map[string]any)
			a = answer{status: r.Status, retry: r.Header.Get("Retry-After") != ""}
			a.code, _ = e["code"].(string)
			a.message, _ = e["message"].(string)
			if a.status != http.StatusServiceUnavailable || a.code != "service_unavailable" || !a.retry {
				t.Errorf("%s, failed start #%d: %d %s Retry-After=%v: %s", impl, i, a.status, a.code, a.retry, r.Body)
			}
		}
		if err := fsguard.RemoveAll(broken); err != nil {
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
