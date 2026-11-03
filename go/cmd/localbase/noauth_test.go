package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

func TestHostIsLoopback(t *testing.T) {
	names := map[string][]net.IP{
		"only-loopback.test": {net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		"mixed.test":         {net.IPv4(127, 0, 0, 1), net.IPv4(192, 168, 1, 9)},
		"lan.test":           {net.IPv4(192, 168, 1, 9)},
		"nothing.test":       {},
	}
	old := lookupHost
	lookupHost = func(_ context.Context, host string) ([]net.IP, error) {
		if ips, ok := names[host]; ok {
			return ips, nil
		}
		return nil, errors.New("no such host")
	}
	t.Cleanup(func() { lookupHost = old })
	for host, want := range map[string]bool{
		"127.0.0.1":          true,
		"127.1.2.3":          true,
		"::1":                true,
		"[::1]":              true,
		"localhost":          true,
		"LOCALHOST.":         true,
		"only-loopback.test": true,
		"":                   false, // binds every interface
		" ":                  false,
		"0.0.0.0":            false,
		"::":                 false,
		"[::]":               false,
		"192.168.1.9":        false,
		"10.0.0.1":           false,
		"fe80::1":            false,
		"mixed.test":         false, // one reachable address is enough
		"lan.test":           false,
		"nothing.test":       false,
		"unresolvable.test":  false,
	} {
		if got := hostIsLoopback(host); got != want {
			t.Errorf("hostIsLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestNoAuthListenerError(t *testing.T) {
	for addr, ok := range map[string]bool{"127.0.0.1:1": true, "[::1]:1": true, "0.0.0.0:1": false, "[::]:1": false, "192.168.1.9:1": false} {
		a, err := net.ResolveTCPAddr("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if got := noAuthListenerError(a) == nil; got != ok {
			t.Errorf("%s: allowed = %v, want %v", addr, got, ok)
		}
	}
	if noAuthListenerError(&net.UnixAddr{Name: "/tmp/x"}) == nil {
		t.Error("a non-TCP listener was allowed")
	}
}

// noAuthRoot is an initialized root.
func noAuthRoot(t *testing.T) string {
	t.Helper()
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	return root
}

func refused(t *testing.T, code int, out, host string) {
	t.Helper()
	if code == 0 || !strings.Contains(out, "Refusing --no-auth") || !strings.Contains(out, host) || !strings.Contains(out, "--host 127.0.0.1") {
		t.Errorf("%s: code %d, output:\n%s", host, code, out)
	}
}

// busyPort is a port the test holds, so a serve that passes the --no-auth check
// fails at the bind rather than serving.
func busyPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

// `serve --no-auth` refuses every way of asking for a non-loopback gateway,
// before it binds anything.
func TestServeRefusesNoAuthBeyondLoopback(t *testing.T) {
	root := noAuthRoot(t)
	port := busyPort(t)
	for _, host := range []string{"0.0.0.0", "::", "192.168.1.9"} {
		code, out := runApp(t, "--root", root, "serve", "--no-auth", "--no-llm", "--host", host, "--port", port)
		refused(t, code, out, host)
	}
	// The saved gateway host, which only a TOML `config apply` can set.
	_, doc := runApp(t, "--root", root, "config", "show")
	const loopback = "[gateway]\nhost = \"127.0.0.1\"\n"
	if !strings.Contains(doc, loopback) {
		t.Fatalf("config show:\n%s", doc)
	}
	start := strings.Index(doc, "# Secrets")
	if start < 0 {
		start = 0
	}
	file := filepath.Join(t.TempDir(), "config.toml")
	body := strings.Replace(doc[start:], loopback, "[gateway]\nhost = \"0.0.0.0\"\n", 1)
	if i := strings.Index(body, "License:"); i > 0 {
		body = body[:i]
	}
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := runApp(t, "--root", root, "config", "apply", "--file", file, "--restart", "never"); code != 0 {
		t.Fatalf("config apply: %s", out)
	}
	code, out := runApp(t, "--root", root, "serve", "--no-auth", "--no-llm", "--port", port)
	refused(t, code, out, "0.0.0.0")
	// An empty --host means the saved host, so it is refused here too.
	code, out = runApp(t, "--root", root, "serve", "--no-auth", "--no-llm", "--host", "", "--port", port)
	refused(t, code, out, "0.0.0.0")
	// The flag can still bring it back to loopback: past the check, to the bind.
	code, out = runApp(t, "--root", root, "serve", "--no-auth", "--no-llm", "--host", "127.0.0.1", "--port", port)
	if code == 0 || strings.Contains(out, "Refusing --no-auth") {
		t.Errorf("--host 127.0.0.1 over a wide saved host: code %d, output:\n%s", code, out)
	}
}

// The default host is loopback, so a plain --no-auth passes the check, as far as
// the bind of the held port (contract/noauth_test.go serves on the other loopback
// names for real, which here would not collide with it). The
// runtime host settings only reach the backends, which always bind loopback, so
// they cannot widen a --no-auth gateway and do not refuse it.
func TestServeAllowsNoAuthOnLoopback(t *testing.T) {
	root := noAuthRoot(t)
	port := busyPort(t)
	for name, args := range map[string][]string{
		"default":         {},
		"empty":           {"--host", ""},
		"127.0.0.1":       {"--host", "127.0.0.1"},
		"wide --llm-host": {"--llm-host", "0.0.0.0"},
	} {
		code, out := runApp(t, append([]string{"--root", root, "serve", "--no-auth", "--no-llm", "--port", port}, args...)...)
		if strings.Contains(out, "Refusing --no-auth") || code == 0 {
			t.Errorf("%s: code %d, output:\n%s", name, code, out)
		}
	}
	t.Setenv("LOCALBASE_HOST", "0.0.0.0")
	t.Setenv("LOCALBASE_STT_HOST", "0.0.0.0")
	if code, out := runApp(t, "--root", root, "serve", "--no-auth", "--no-llm", "--port", port); strings.Contains(out, "Refusing --no-auth") || code == 0 {
		t.Errorf("env hosts: code %d, output:\n%s", code, out)
	}
}

// A saved empty host would bind every interface.
func TestServeRefusesNoAuthOnEmptySavedHost(t *testing.T) {
	root := noAuthRoot(t)
	st, err := store.Open(filepath.Join(root, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`UPDATE config SET gateway_host = ''`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	code, out := runApp(t, "--root", root, "serve", "--no-auth", "--no-llm", "--port", busyPort(t))
	if code == 0 || !strings.Contains(out, "Refusing --no-auth") {
		t.Errorf("code %d, output:\n%s", code, out)
	}
}

// With authentication on, the host is the person's choice, as before; the
// refusal is about --no-auth only.
func TestServeWithAuthIgnoresTheLoopbackRule(t *testing.T) {
	root := noAuthRoot(t)
	code, out := runApp(t, "--root", root, "serve", "--host", "not-a-real-host.invalid", "--no-llm", "--port", busyPort(t))
	if code == 0 || strings.Contains(out, "Refusing --no-auth") {
		t.Errorf("code %d, output:\n%s", code, out)
	}
}
