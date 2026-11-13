package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// logLines runs write with a logger and returns each line it wrote as "LEVEL event count msg".
func logLines(t *testing.T, write func(*slog.Logger)) []string {
	t.Helper()
	var buf bytes.Buffer
	write(slog.New(slog.NewJSONHandler(&buf, nil)))
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatal(err)
		}
		count, _ := v["count"].(float64)
		out = append(out, v["level"].(string)+" "+v["event"].(string)+" "+itoa(int(count))+" "+v["msg"].(string))
	}
	return out
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// What serve says at start about bindings that name no provider: with one provider they are
// adopted, and said so; with two, what is left is said once per kind.
func TestNameBindingIssuersSaysWhatItDid(t *testing.T) {
	ctx := context.Background()
	t.Run("one provider", func(t *testing.T) {
		st := testStore(t)
		oldRows(t, st)
		got := logLines(t, func(l *slog.Logger) { NameBindingIssuers(ctx, st, direct(t, "https://a.example.com"), l) })
		want := []string{"INFO auth.issuers-adopted 3 Email bindings and invitations now name their sign-in provider."}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
	t.Run("two providers", func(t *testing.T) {
		st := testStore(t)
		oldRows(t, st)
		got := logLines(t, func(l *slog.Logger) {
			NameBindingIssuers(ctx, st, direct(t, "https://a.example.com", "https://b.example.com"), l)
		})
		want := []string{
			"WARN auth.bindings-unbound 1 1 email bindings are not bound to a sign-in provider, so they grant nothing. Run: local-base access bindings adopt --issuer <issuer>",
			"WARN auth.bindings-unbound 1 1 domain bindings are not bound to a sign-in provider, so they grant nothing. Run: local-base access bindings adopt --issuer <issuer>",
			"WARN auth.bindings-unbound 1 1 invitations are not bound to a sign-in provider, so they grant nothing. Run: local-base access bindings adopt --issuer <issuer>",
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
	t.Run("no provider", func(t *testing.T) {
		st := testStore(t)
		oldRows(t, st)
		if got := logLines(t, func(l *slog.Logger) { NameBindingIssuers(ctx, st, nil, l) }); len(got) != 0 {
			t.Errorf("with no provider: %q", got)
		}
	})
}
