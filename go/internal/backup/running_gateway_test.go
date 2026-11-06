package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// What restore says about a gateway on the root depends on what can stop it, and on which root it is.
// `stop` only controls the service, and refuses a gateway started with serve, so naming it there would
// send someone to a command that fails. Every command names the root it was given.
func TestARunningGatewayIsNamedForWhatStopsIt(t *testing.T) {
	src, _ := newRoot(t, dir(t))
	file := create(t, src, false).Output
	const note = " This is the default root, the one your installed LocalBase uses."
	service := func(pid int, path, note string) string {
		return fmt.Sprintf("A gateway (pid %d) is running on %s.%s Stop it first: local-base --root '%s' stop. That also disables the service, until you run: local-base --root '%s' start", pid, path, note, path, path)
	}
	foreground := func(pid int, path, note string) string {
		return fmt.Sprintf("A gateway (pid %d) started with local-base serve is running on %s.%s Stop it where it is running, with Ctrl-C or by ending that process. The service commands don't control it.", pid, path, note)
	}
	for _, c := range []struct {
		name        string
		defaultRoot bool
		serviceID   string
		want        func(pid int, path, note string) string
		note        string
	}{
		{"another root, the service", false, "com.localbase.gateway.test", service, ""},
		{"the default root, the service", true, "com.localbase.gateway.test", service, note},
		{"another root, in the foreground", false, "", foreground, ""},
		{"the default root, in the foreground", true, "", foreground, note},
	} {
		t.Run(c.name, func(t *testing.T) {
			home, err := filepath.EvalSymlinks(dir(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("LOCALBASE_ROOT", "")
			path := filepath.Join(home, "a root of its own")
			if c.defaultRoot {
				path = filepath.Join(home, ".local", "share", "local-base")
			}
			target, err := root.Init(path)
			if err != nil {
				t.Fatal(err)
			}
			token := ""
			if c.serviceID != "" {
				token = "a-token"
			}
			unlock, err := target.LockAs("127.0.0.1", 1, c.serviceID, token)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			s, err := Stage(context.Background(), file, target.Dir, true)
			if err == nil {
				s.Discard()
				t.Fatal("it was accepted, with a gateway running")
			}
			if want := c.want(os.Getpid(), target.Dir, c.note); err.Error() != want {
				t.Errorf("it said:\n%s\nand should say:\n%s", err, want)
			}
		})
	}
}
