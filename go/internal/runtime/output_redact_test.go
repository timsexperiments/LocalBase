package runtime

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// llama-server names the image URL a request sent, at every verbosity. A URL can carry
// a signed token or a password, and at debug level this line reaches the event log, the
// diagnostics bundle and any exporter.
func TestBackendOutputRedactsURLs(t *testing.T) {
	var logs syncBuffer
	script := `echo "0.03.012 I srv  handle_media: downloading image from 'https://user:hunter2pw@example.test/a.png?sig=SECRETTOKEN&x=1#frag'"; echo "listening on http://127.0.0.1:8080"`
	p, err := StartProcess("child", "/bin/sh", []string{"-c", script}, nil, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the child never ended")
	}
	var got string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		got = logs.String()
		if strings.Contains(got, "127.0.0.1:8080") && strings.Contains(got, "a.png") {
			break
		}
	}
	for _, secret := range []string{"hunter2pw", "SECRETTOKEN", "user:", "frag"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q is in the log:\n%s", secret, got)
		}
	}
	for _, kept := range []string{"example.test/a.png", "sig=REDACTED", "http://127.0.0.1:8080"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q should stay:\n%s", kept, got)
		}
	}
}

func TestRedactOutputLeavesOtherTextAlone(t *testing.T) {
	line := "0.00.705 I load_tensors: offloaded 0/29 layers to GPU; ratio 1:2, see foo://"
	if got := redactOutputURLs(line); got != line {
		t.Errorf("%q", got)
	}
}
