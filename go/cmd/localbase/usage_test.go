package main

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
)

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// The table is buffered until the end, so this is the only place a failed write can show. Without
// it the person gets a report with no table and an exit status that says all went well.
func TestUsageTableThatCannotBeWrittenIsAnError(t *testing.T) {
	r := &usage.Report{Groups: []usage.Group{{}}}
	if err := printUsage(&cli.Ctx{Stdout: brokenWriter{}, Stderr: io.Discard}, r); err == nil {
		t.Fatal("a table nobody received should be an error")
	}
	if err := printUsage(&cli.Ctx{Stdout: io.Discard, Stderr: io.Discard}, r); err != nil {
		t.Fatalf("a table that was written: %v", err)
	}
}

// Stopping the service disables it, so advice to do that has to say so, and how to undo it.
func TestStopAdviceSaysWhatItDoes(t *testing.T) {
	said := stopGatewayAdvice("/it's", &root.Owner{ServiceID: "a-service"})
	for _, want := range []string{"--root '/it'\\''s' stop", "disables the service", "--root '/it'\\''s' start"} {
		if !strings.Contains(said, want) {
			t.Errorf("missing %q in: %s", want, said)
		}
	}
}

// Every command names the root it was given, the default root is said in words, and a gateway started
// with serve isn't sent to `stop`, which only controls the service and would refuse it.
func TestStopAdviceFitsTheRootAndTheGateway(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LOCALBASE_ROOT", "")
	def := filepath.Join(home, ".local", "share", "local-base")
	const note = " This is the default root, the one your installed LocalBase uses."
	service := &root.Owner{ServiceID: "a-service"}
	foreground := &root.Owner{}
	for _, c := range []struct {
		name  string
		dir   string
		owner *root.Owner
		want  string
	}{
		{"another root, the service", "/a/root", service,
			" Nothing changed. Try again, or stop the gateway first: local-base --root '/a/root' stop. That also disables the service, until you run: local-base --root '/a/root' start"},
		{"the default root, the service", def, service,
			" Nothing changed." + note + " Try again, or stop the gateway first: local-base --root '" + def + "' stop. That also disables the service, until you run: local-base --root '" + def + "' start"},
		{"another root, in the foreground", "/a/root", foreground,
			" Nothing changed. Try again, or stop the gateway where it is running, with Ctrl-C or by ending that process. The service commands don't control it."},
		{"the default root, in the foreground", def, foreground,
			" Nothing changed." + note + " Try again, or stop the gateway where it is running, with Ctrl-C or by ending that process. The service commands don't control it."},
	} {
		if got := stopGatewayAdvice(c.dir, c.owner); got != c.want {
			t.Errorf("%s:\n%s\nand should say:\n%s", c.name, got, c.want)
		}
	}
}
