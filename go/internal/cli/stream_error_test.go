package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// A streaming command's error (serve --json, logs --follow --json) is bounded and redacted like
// every other CLI error: the JSON line is no way around fail()'s BoundedText (review 01-M5).
func TestAStreamingErrorIsRedactedLikeAnyOther(t *testing.T) {
	secret := "Bearer sk-abcdefghijklmnopqrstuvwxyz0123456789"
	cmd := &Command{Name: "stream", Run: func(c *Ctx) (any, error) {
		c.Streaming()
		return nil, errors.New("the backend said: " + secret + " " + strings.Repeat("x", 5000))
	}}
	var out, errb bytes.Buffer
	app := &App{Name: "x", Root: &Command{Sub: []*Command{cmd}}, Stdout: &out, Stderr: &errb}
	if code := app.Run([]string{"--json", "stream"}); code != 1 {
		t.Fatalf("exit %d", code)
	}
	var line struct {
		Event string            `json:"event"`
		Error map[string]string `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &line); err != nil || line.Event != "error" {
		t.Fatalf("not one error line: %v %q", err, out.String())
	}
	msg := line.Error["message"]
	if strings.Contains(msg, "sk-abcdefghij") {
		t.Errorf("a credential reached the JSON line: %.80q", msg)
	}
	if n := utf8.RuneCountInString(msg); n > 2048 {
		t.Errorf("a %d-character message; the bound is 2048", n)
	}
}
