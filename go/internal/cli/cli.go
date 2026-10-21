// Package cli is LocalBase's command framework: a command tree, GNU-style
// flags, and the TS CLI's output contract.
//
//   - Global options (--root, --json, --non-interactive) may appear anywhere.
//   - --json prints one envelope on stdout: {"ok":true,"data":...} or
//     {"ok":false,"error":{"code","message"}}; human text goes to stderr.
//   - Exit codes: 0 success, 1 operational failure, 2 invalid input.
//   - Options: --name=value, --name value, --flag, --no-flag; -y style aliases.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

// InputError is invalid command input (exit 2, code invalid_input).
type InputError struct{ Message string }

func (e *InputError) Error() string { return e.Message }

// Inputf returns an InputError.
func Inputf(format string, a ...any) error { return &InputError{fmt.Sprintf(format, a...)} }

// Option describes one flag.
type Option struct {
	Name       string
	Alias      string // single letter, used as -x
	Bool       bool
	Repeatable bool
	Value      string // placeholder in help, such as <path>
	Help       string
	Default    string
}

// Global options every command accepts.
type Global struct {
	Root           string
	JSON           bool
	NonInteractive bool
}

// Ctx is a command invocation.
type Ctx struct {
	Global      Global
	Args        []string // positionals
	values      map[string][]string
	bools       map[string]bool
	Stdout      io.Writer
	Stderr      io.Writer
	streamingOK bool
}

// String returns the last value of an option, or its default.
func (c *Ctx) String(name string) string {
	if v := c.values[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

// Has reports whether the option was given (a value option or a flag in
// either polarity).
func (c *Ctx) Has(name string) bool {
	_, v := c.values[name]
	_, b := c.bools[name]
	return v || b
}

// Strings returns every value of a repeatable option.
func (c *Ctx) Strings(name string) []string { return c.values[name] }

// Bool returns a flag's value (false when absent).
func (c *Ctx) Bool(name string) bool { return c.bools[name] }

// OptBool returns a flag's value and whether it was given.
func (c *Ctx) OptBool(name string) (bool, bool) {
	v, ok := c.bools[name]
	return v, ok
}

// Int parses an integer option; ok is false when absent.
func (c *Ctx) Int(name string) (n int, ok bool, err error) {
	s := c.String(name)
	if !c.Has(name) {
		return 0, false, nil
	}
	n, err = strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, true, Inputf("--%s must be an integer", name)
	}
	return n, true, nil
}

// Info prints human output: stdout normally, stderr under --json so stdout
// carries only the envelope.
func (c *Ctx) Info(format string, a ...any) {
	w := c.Stdout
	if c.Global.JSON {
		w = c.Stderr
	}
	fmt.Fprintf(w, format+"\n", a...)
}

// Error prints a human message on stderr, with or without --json (TS
// output.error).
func (c *Ctx) Error(format string, a ...any) {
	fmt.Fprintf(c.Stderr, format+"\n", a...)
}

// Streaming marks a command that writes its own JSON lines (logs --follow,
// serve lifecycle events), so no envelope is printed on success.
func (c *Ctx) Streaming() { c.streamingOK = true }

// Line writes one JSON value as a line on stdout (streaming commands).
func (c *Ctx) Line(v any) {
	b, _ := json.Marshal(v)
	c.Stdout.Write(append(b, '\n'))
}

// Command is a leaf command or a group (Run nil, Sub set).
type Command struct {
	Name        string
	Summary     string
	Usage       string // positional usage, such as "<model-id>"
	Options     []Option
	MinArgs     int
	MaxArgs     int
	Examples    []string
	Run         func(c *Ctx) (any, error)
	Sub         []*Command
	ExitCodeFor func(data any) int // optional: non-zero exit with a success envelope (doctor, smoke)
}

func (c *Command) find(name string) *Command {
	for _, s := range c.Sub {
		if s.Name == name {
			return s
		}
	}
	return nil
}

var globalOptions = []Option{
	{Name: "root", Value: "<path>", Help: "LocalBase data directory"},
	{Name: "non-interactive", Bool: true, Help: "Never open interactive prompts"},
	{Name: "json", Bool: true, Help: "Emit machine-readable JSON"},
}

// App is a CLI program.
type App struct {
	Name    string
	Version string
	Summary string
	Root    *Command
	// Default runs when no command is given (TS: configure).
	Default string
	Stdout  io.Writer
	Stderr  io.Writer
}

func (a *App) writeEnvelope(v any) {
	b, _ := json.Marshal(v)
	a.Stdout.Write(append(b, '\n'))
}

func (a *App) fail(json bool, code, message string, exit int, help *Command, path []string) int {
	// TS redacts every CLI error message like an external log text.
	message = obs.BoundedText(message, 2048)
	if json {
		a.writeEnvelope(map[string]any{"ok": false, "error": map[string]string{"code": code, "message": message}})
	}
	fmt.Fprintf(a.Stderr, "Error: %s\n", message)
	if help != nil {
		fmt.Fprintln(a.Stderr, a.help(help, path))
	}
	return exit
}

// Run executes args and returns the exit code.
func (a *App) Run(args []string) int {
	if a.Stdout == nil {
		a.Stdout = os.Stdout
	}
	if a.Stderr == nil {
		a.Stderr = os.Stderr
	}
	g, rest, err := splitGlobals(args)
	if err != nil {
		return a.fail(hasJSON(args), "invalid_input", err.Error(), 2, a.Root, nil)
	}
	if len(rest) == 1 && (rest[0] == "--version" || rest[0] == "-v") {
		if g.JSON {
			a.writeEnvelope(map[string]any{"ok": true, "data": map[string]string{"version": a.Version}})
		} else {
			fmt.Fprintln(a.Stdout, a.Version)
		}
		return 0
	}
	cmd, path := a.Root, []string{}
	i := 0
	for ; i < len(rest); i++ {
		tok := rest[i]
		if strings.HasPrefix(tok, "-") || cmd.Run != nil {
			break
		}
		next := cmd.find(tok)
		if next == nil {
			break
		}
		cmd, path = next, append(path, tok)
	}
	rest = rest[i:]
	if helpRequested(rest) {
		return a.printHelp(g.JSON, cmd, path)
	}
	if cmd.Run == nil {
		switch {
		case len(path) == 0 && (len(rest) == 0 || strings.HasPrefix(rest[0], "-")) && a.Default != "":
			cmd, path = a.Root.find(a.Default), []string{a.Default}
		case len(rest) == 0:
			return a.printHelp(g.JSON, cmd, path)
		default:
			return a.fail(g.JSON, "invalid_input", "Unknown command: "+rest[0], 2, cmd, path)
		}
	}
	ctx := &Ctx{Global: g, Stdout: a.Stdout, Stderr: a.Stderr}
	if err := parseOptions(ctx, cmd, rest); err != nil {
		return a.fail(g.JSON, "invalid_input", err.Error(), 2, cmd, path)
	}
	data, err := cmd.Run(ctx)
	if err != nil {
		var ie *InputError
		if errors.As(err, &ie) {
			return a.fail(g.JSON, "invalid_input", ie.Message, 2, cmd, path)
		}
		if ctx.streamingOK && g.JSON {
			ctx.Line(map[string]any{"event": "error", "error": map[string]string{"code": "operational_error", "message": err.Error()}})
			return 1
		}
		return a.fail(g.JSON, "operational_error", err.Error(), 1, nil, path)
	}
	if g.JSON && !ctx.streamingOK {
		if data == nil {
			data = map[string]any{}
		}
		a.writeEnvelope(map[string]any{"ok": true, "data": data})
	}
	if cmd.ExitCodeFor != nil {
		return cmd.ExitCodeFor(data)
	}
	return 0
}

func hasJSON(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--json" {
			return true
		}
	}
	return false
}

func helpRequested(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

// splitGlobals removes the global options from anywhere before "--".
func splitGlobals(args []string) (Global, []string, error) {
	var g Global
	var rest []string
	literal := false
	for i := 0; i < len(args); i++ {
		t := args[i]
		if t == "--" {
			literal = true
		}
		if literal {
			rest = append(rest, t)
			continue
		}
		switch {
		case t == "--json":
			g.JSON, g.NonInteractive = true, true
		case t == "--non-interactive":
			g.NonInteractive = true
		case strings.HasPrefix(t, "--json=") || strings.HasPrefix(t, "--non-interactive="):
			return g, nil, Inputf("%s does not accept a value", strings.SplitN(t, "=", 2)[0])
		case t == "--root":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return g, nil, Inputf("--root requires a value")
			}
			g.Root = args[i+1]
			i++
		case strings.HasPrefix(t, "--root="):
			g.Root = strings.TrimPrefix(t, "--root=")
		default:
			rest = append(rest, t)
		}
	}
	return g, rest, nil
}

func parseOptions(c *Ctx, cmd *Command, args []string) error {
	c.values, c.bools = map[string][]string{}, map[string]bool{}
	byName := map[string]Option{}
	for _, o := range cmd.Options {
		byName["--"+o.Name] = o
		if o.Alias != "" {
			byName["-"+o.Alias] = o
		}
	}
	literal := false
	for i := 0; i < len(args); i++ {
		t := args[i]
		if t == "--" && !literal {
			literal = true
			continue
		}
		if literal || !strings.HasPrefix(t, "-") || t == "-" {
			c.Args = append(c.Args, t)
			continue
		}
		name, value, hasValue := strings.Cut(t, "=")
		if strings.HasPrefix(name, "--no-") {
			o, ok := byName["--"+strings.TrimPrefix(name, "--no-")]
			if !ok || !o.Bool {
				return Inputf("Unknown option: %s", name)
			}
			if hasValue {
				return Inputf("%s does not accept a value", name)
			}
			c.bools[o.Name] = false
			continue
		}
		o, ok := byName[name]
		if !ok {
			return Inputf("Unknown option: %s", name)
		}
		if o.Bool {
			if hasValue {
				return Inputf("%s does not accept a value", name)
			}
			c.bools[o.Name] = true
			continue
		}
		if !hasValue {
			if i+1 >= len(args) || (strings.HasPrefix(args[i+1], "-") && args[i+1] != "-") {
				return Inputf("%s requires a value", name)
			}
			value = args[i+1]
			i++
		}
		if o.Repeatable {
			c.values[o.Name] = append(c.values[o.Name], value)
		} else {
			c.values[o.Name] = []string{value}
		}
	}
	for _, o := range cmd.Options {
		if o.Default != "" && !c.Has(o.Name) {
			c.values[o.Name] = []string{o.Default}
		}
	}
	if len(c.Args) < cmd.MinArgs || len(c.Args) > cmd.MaxArgs {
		return Inputf("Invalid positional arguments for %s", cmd.Name)
	}
	return nil
}

func (a *App) printHelp(json bool, cmd *Command, path []string) int {
	text := a.help(cmd, path)
	if json {
		a.writeEnvelope(map[string]any{"ok": true, "data": map[string]string{"help": text}})
	} else {
		fmt.Fprintln(a.Stdout, text)
	}
	return 0
}

func (a *App) help(cmd *Command, path []string) string {
	var b strings.Builder
	if len(path) == 0 {
		fmt.Fprintf(&b, "%s (%s v%s)\n\n", a.Summary, a.Name, a.Version)
	} else if cmd.Summary != "" {
		fmt.Fprintf(&b, "%s\n\n", cmd.Summary)
	}
	usage := append([]string{a.Name}, path...)
	usage = append(usage, "[OPTIONS]")
	if len(cmd.Sub) > 0 {
		names := make([]string, len(cmd.Sub))
		for i, s := range cmd.Sub {
			names[i] = s.Name
		}
		usage = append(usage, strings.Join(names, "|"))
	}
	if cmd.Usage != "" {
		usage = append(usage, cmd.Usage)
	}
	fmt.Fprintf(&b, "USAGE %s\n\nOPTIONS\n\n", strings.Join(usage, " "))
	opts := append(slices.Clone(globalOptions), cmd.Options...)
	width := 0
	label := func(o Option) string {
		l := "--" + o.Name
		if o.Alias != "" {
			l = "-" + o.Alias + ", " + l
		}
		if !o.Bool {
			v := o.Value
			if v == "" {
				v = "<value>"
			}
			l += "=" + v
		}
		return l
	}
	for _, o := range opts {
		width = max(width, len(label(o)))
	}
	for _, o := range opts {
		fmt.Fprintf(&b, "  %*s    %s\n", width, label(o), o.Help)
	}
	if len(cmd.Sub) > 0 {
		b.WriteString("\nCOMMANDS\n\n")
		w := 0
		for _, s := range cmd.Sub {
			w = max(w, len(s.Name))
		}
		for _, s := range cmd.Sub {
			fmt.Fprintf(&b, "  %*s    %s\n", w, s.Name, s.Summary)
		}
		fmt.Fprintf(&b, "\nUse %s <command> --help for more information about a command.\n", strings.Join(append([]string{a.Name}, path...), " "))
	}
	if len(cmd.Examples) > 0 {
		b.WriteString("\nEXAMPLES\n\n")
		for _, e := range cmd.Examples {
			fmt.Fprintf(&b, "  %s\n", e)
		}
	}
	b.WriteString("\nLicense: AGPL-3.0-or-later · Source: https://github.com/timsexperiments/LocalBase")
	return b.String()
}

// SplitList splits a comma-separated option into trimmed, non-empty values.
func SplitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// SortedKeys returns a map's keys in order.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
