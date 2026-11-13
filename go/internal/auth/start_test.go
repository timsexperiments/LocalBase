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

func TestLocalLinkBase(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1:2273": "http://127.0.0.1:2273",
		"0.0.0.0:2273":   "http://127.0.0.1:2273",
		"[::]:2273":      "http://127.0.0.1:2273",
		"localhost:2273": "http://localhost:2273",
		"[::1]:2273":     "http://[::1]:2273",
	} {
		if got, err := localLinkBase(addr); err != nil || got != want {
			t.Errorf("%s: %q %v", addr, got, err)
		}
	}
	if _, err := localLinkBase("192.168.1.5:2273"); err == nil {
		t.Error("a gateway with no loopback listener cannot redeem a link")
	}
}

// A link is the base, the redeem path and the token, and its id is the token's.
func TestLocalLinkURL(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	url, id, err := LocalLinkURL(ctx, st, "0.0.0.0:2273")
	if err != nil {
		t.Fatal(err)
	}
	prefix := "http://127.0.0.1:2273" + LocalOncePath + "#"
	token, ok := strings.CutPrefix(url, prefix)
	if !ok || token == "" || id != LocalLinkID(token) {
		t.Errorf("url %q, id %q", url, id)
	}
	if _, _, err := LocalLinkURL(ctx, st, "192.168.1.5:2273"); err == nil {
		t.Error("a non-loopback listener issued a link")
	}
}

// The first-run link is issued once per root: the second ask gets nothing and issues nothing.
func TestFirstRunLinkIsShownOnce(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	url, id, err := FirstRunLink(ctx, st, "127.0.0.1:2273")
	if err != nil {
		t.Fatal(err)
	}
	token, ok := strings.CutPrefix(url, "http://127.0.0.1:2273"+LocalOncePath+"#")
	if !ok || token == "" || id != LocalLinkID(token) {
		t.Errorf("first: url %q, id %q", url, id)
	}
	if _, shown, err := st.Setting(ctx, firstRunShownKey); err != nil || !shown {
		t.Errorf("not recorded as shown (%v)", err)
	}
	if url, id, err := FirstRunLink(ctx, st, "127.0.0.1:2273"); url != "" || id != "" || err != nil {
		t.Errorf("second: url %q, id %q, err %v", url, id, err)
	}
}
