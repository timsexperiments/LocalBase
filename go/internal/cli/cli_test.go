package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func testApp(run func(c *Ctx) (any, error)) (*App, *bytes.Buffer, *bytes.Buffer) {
	out, errb := &bytes.Buffer{}, &bytes.Buffer{}
	leaf := &Command{Name: "create", Summary: "Create", MaxArgs: 1, Run: run, Options: []Option{
		{Name: "name", Value: "<name>"}, {Name: "scope", Repeatable: true}, {Name: "auth", Bool: true}, {Name: "yes", Alias: "y", Bool: true}}}
	root := &Command{Sub: []*Command{{Name: "keys", Summary: "Keys", Sub: []*Command{leaf}}}}
	return &App{Name: "local-base", Version: "1.2.3", Root: root, Stdout: out, Stderr: errb}, out, errb
}

func TestParsing(t *testing.T) {
	var got *Ctx
	app, out, _ := testApp(func(c *Ctx) (any, error) { got = c; return map[string]any{"ok": 1}, nil })
	code := app.Run([]string{"keys", "--json", "create", "--name", "a", "--scope=x", "--scope", "y", "--no-auth", "-y", "pos", "--root=/r"})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got.String("name") != "a" || strings.Join(got.Strings("scope"), ",") != "x,y" || got.Bool("auth") || !got.Bool("yes") {
		t.Errorf("parsed %+v %+v", got.values, got.bools)
	}
	if v, ok := got.OptBool("auth"); v || !ok {
		t.Error("--no-auth not recorded")
	}
	if got.Global.Root != "/r" || !got.Global.JSON || got.Args[0] != "pos" {
		t.Errorf("globals %+v args %v", got.Global, got.Args)
	}
	var env map[string]any
	if json.Unmarshal(out.Bytes(), &env) != nil || env["ok"] != true {
		t.Errorf("envelope %q", out.String())
	}
}

func TestInputErrors(t *testing.T) {
	app, out, errb := testApp(func(c *Ctx) (any, error) { return nil, nil })
	for _, args := range [][]string{{"keys", "create", "--bogus"}, {"keys", "create", "--name"}, {"keys", "create", "--auth=1"},
		{"keys", "nope"}, {"keys", "create", "a", "b"}} {
		out.Reset()
		errb.Reset()
		if code := app.Run(append(args, "--json")); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
		if !strings.Contains(out.String(), `"code":"invalid_input"`) {
			t.Errorf("%v: %q", args, out.String())
		}
	}
	app2, out2, _ := testApp(func(c *Ctx) (any, error) { return nil, errFake })
	if code := app2.Run([]string{"keys", "create", "--json"}); code != 1 || !strings.Contains(out2.String(), "operational_error") {
		t.Errorf("operational: %d %q", code, out2.String())
	}
}

type fakeErr struct{}

func (fakeErr) Error() string { return "boom" }

var errFake = fakeErr{}

func TestHelpAndVersion(t *testing.T) {
	app, out, _ := testApp(nil)
	if app.Run([]string{"--version"}) != 0 || strings.TrimSpace(out.String()) != "1.2.3" {
		t.Errorf("version %q", out.String())
	}
	out.Reset()
	if app.Run([]string{"keys", "--help"}) != 0 || !strings.Contains(out.String(), "COMMANDS") {
		t.Errorf("help %q", out.String())
	}
}

// An id can start with '-': a session id is base64url, so about 1 in 64 did. `--` always
// makes the next token a value, and a command that declares its one positional as an id
// takes a token that is no option as that id. A revoke that fails to parse leaves a leaked
// session working.
func TestDashValues(t *testing.T) {
	run := func(cmd *Command, args ...string) (*Ctx, int, string) {
		var got *Ctx
		cmd.Run = func(c *Ctx) (any, error) { got = c; return map[string]any{}, nil }
		var out, errb bytes.Buffer
		app := &App{Name: "x", Root: &Command{Sub: []*Command{cmd}}, Stdout: &out, Stderr: &errb}
		code := app.Run(append([]string{cmd.Name}, args...))
		return got, code, out.String() + errb.String()
	}
	plain := func() *Command {
		return &Command{Name: "plain", Usage: "<id>", MinArgs: 1, MaxArgs: 1, Options: []Option{{Name: "flag", Bool: true}}}
	}
	id := func() *Command {
		return &Command{Name: "id", Usage: "<id>", MinArgs: 1, MaxArgs: 1, IDArg: true, Options: []Option{{Name: "flag", Bool: true}}}
	}
	if c, code, _ := run(plain(), "--", "-AnZqiOlvvn8"); code != 0 || c.Args[0] != "-AnZqiOlvvn8" {
		t.Errorf("plain with --: %d %v", code, c)
	}
	if _, code, msg := run(plain(), "-AnZqiOlvvn8"); code == 0 || !strings.Contains(msg, "write -- before it") || !strings.Contains(msg, "Unknown option") {
		t.Errorf("plain without --: %d %q", code, msg)
	}
	for _, v := range []string{"-AnZqiOlvvn8", "--AnZqiOlvvn8", "-x=y"} {
		if c, code, msg := run(id(), v); code != 0 || c.Args[0] != v {
			t.Errorf("id %q: %d %q", v, code, msg)
		}
		if c, code, msg := run(id(), "--", v); code != 0 || c.Args[0] != v {
			t.Errorf("id %q with --: %d %q", v, code, msg)
		}
	}
	// A real option stays an option, and a second token is still too many.
	if c, code, msg := run(id(), "--flag", "abc"); code != 0 || !c.Bool("flag") || c.Args[0] != "abc" {
		t.Errorf("real option: %d %q", code, msg)
	}
	if _, code, _ := run(id(), "-a", "-b"); code == 0 {
		t.Error("two ids accepted")
	}
}

// A runnable command can also have subcommands: a known name goes to the subcommand, and
// anything else stays the parent's argument, as before.
func TestRunnableCommandWithSubcommands(t *testing.T) {
	var ran []string
	sub := &Command{Name: "clear", Run: func(c *Ctx) (any, error) { ran = append(ran, "clear"); return nil, nil }}
	parent := &Command{Name: "usage", MaxArgs: 1, Sub: []*Command{sub}, Run: func(c *Ctx) (any, error) {
		ran = append(ran, "usage:"+strings.Join(c.Args, ","))
		return nil, nil
	}}
	a := &App{Name: "x", Version: "1", Root: &Command{Sub: []*Command{parent}}, Stdout: io.Discard, Stderr: io.Discard}
	for _, args := range [][]string{{"usage"}, {"usage", "clear"}, {"usage", "other"}} {
		if code := a.Run(args); code != 0 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if want := "usage:|clear|usage:other"; strings.Join(ran, "|") != want {
		t.Errorf("ran %q, want %q", strings.Join(ran, "|"), want)
	}
}
