package cli

import (
	"bytes"
	"encoding/json"
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
