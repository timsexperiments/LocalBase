package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Every command that takes a positional, walked from the real tree so a new one is
// covered: after `--`, a value that starts with '-' reaches it as that value.
func TestEveryPositionalTakesADashValueAfterTheSeparator(t *testing.T) {
	a := app()
	var walk func(c *cli.Command, path []string)
	seen := 0
	walk = func(c *cli.Command, path []string) {
		if c.MaxArgs >= 1 && c.Run != nil {
			seen++
			var got []string
			real := c.Run
			c.Run = func(x *cli.Ctx) (any, error) { got = x.Args; return map[string]any{}, nil }
			var out, errb bytes.Buffer
			a.Stdout, a.Stderr = &out, &errb
			args := append(append([]string{}, path...), "--", "-AnZqiOlvvn8")
			for i := 1; i < c.MinArgs; i++ {
				args = append(args, "second")
			}
			a.Run(args)
			c.Run = real
			if len(got) == 0 || got[0] != "-AnZqiOlvvn8" {
				t.Errorf("%s: %v, output %s%s", strings.Join(path, " "), got, out.String(), errb.String())
			}
		}
		for _, s := range c.Sub {
			walk(s, append(append([]string{}, path...), s.Name))
		}
	}
	walk(a.Root, nil)
	if seen < 15 {
		t.Errorf("only %d commands with a positional: the walk is not reaching them", seen)
	}
}

// The case that was failing: a session whose id starts with '-' can be revoked, written
// the plain way, and stops working.
func TestRevokeASessionWhoseIDStartsWithADash(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "root")
	r, err := root.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	token, id, err := auth.CreateLocalSession(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	const dashed = "-AnZqiOlvvn8"
	if _, err := st.DB.ExecContext(ctx, `UPDATE local_sessions SET id = ? WHERE id = ?`, dashed, id); err != nil {
		t.Fatal(err)
	}
	a := app()
	var out, errb bytes.Buffer
	a.Stdout, a.Stderr = &out, &errb
	if code := a.Run([]string{"--root", dir, "--json", "access", "sessions", "revoke", dashed}); code != 0 {
		t.Fatalf("revoke: %d %s%s", code, out.String(), errb.String())
	}
	if s, err := auth.FindLocalSession(ctx, st, token); err != nil || s != nil {
		t.Errorf("a revoked session worked: %+v %v", s, err)
	}
}

// New ids cannot be mistaken for options, whoever types them.
func TestNewSessionIDsNeverStartWithADash(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for range 200 {
		_, id, err := auth.CreateLocalSession(context.Background(), st)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(id, "-") {
			t.Fatalf("id %q", id)
		}
		if _, err := auth.RevokeAllLocal(context.Background(), st); err != nil {
			t.Fatal(err)
		}
	}
}

// `revoke --all` ends every session; it takes no id, and needs one or the other.
func TestRevokeAllSessionsCommand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "root")
	r, err := root.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	token, err := auth.CreateSession(ctx, st, "owner", auth.Identity{Issuer: "https://i", Subject: "s"})
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (int, string) {
		a := app()
		var out, errb bytes.Buffer
		a.Stdout, a.Stderr = &out, &errb
		code := a.Run(append([]string{"--root", dir, "--json", "access", "sessions", "revoke"}, args...))
		return code, out.String() + errb.String()
	}
	if code, msg := run(); code == 0 {
		t.Errorf("neither: %s", msg)
	}
	if code, msg := run("--all", "some-id"); code == 0 {
		t.Errorf("both: %s", msg)
	}
	if code, msg := run("--all"); code != 0 || !strings.Contains(msg, `"revoked":1`) {
		t.Fatalf("--all: %d %s", code, msg)
	}
	if s, _ := auth.FindSession(ctx, st, token, map[string]bool{"https://i": true}); s != nil {
		t.Error("a session survived --all")
	}
}
