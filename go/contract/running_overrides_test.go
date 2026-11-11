package contract

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// A real gateway with its LLM port set by the environment: an unrelated management change, then a
// fresh start of the backend, still uses that port. The gateway's own --port isn't configuration,
// so no management change could move it; LOCALBASE_PORT is the kind of override that went missing.
func TestAManagementChangeKeepsARealOverride(t *testing.T) {
	backupGoOnly(t)
	tmp := fsguard.ScratchDir(t)
	e := backupRoot(t, tmp, "root")
	e.key = e.json("keys", "create", "--name", "manager", "--scopes", "inference:chat,models:read,models:manage")["secret"].(string)
	port := testutil.FreePort()
	e.env = append(e.env, fmt.Sprintf("LOCALBASE_PORT=%d", port))
	e.noLLMPortFlag = true
	e.serve()
	defer e.stop()
	listening := func() bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}
	if listening() {
		t.Fatalf("something already listens on %d", port)
	}
	if r := e.chat(backupModel); r.Status != 200 {
		t.Fatalf("first chat: %d %s", r.Status, r.Body)
	}
	if !listening() {
		t.Fatalf("the backend isn't on the overridden port %d", port)
	}
	// Unrelated to the port: this goes through updateConfig.
	if r := e.do("POST", "/_localbase/default-chat-models", `{"models":["`+backupModel+`"]}`); r.Status != 200 {
		t.Fatalf("setting the default chat models: %d %s", r.Status, r.Body)
	}
	if r := e.do("POST", "/_localbase/models/"+backupModel+"/unload", `{}`); r.Status != 200 {
		t.Fatalf("unloading: %d %s", r.Status, r.Body)
	}
	for deadline := time.Now().Add(5 * time.Second); listening(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the backend didn't stop")
		}
	}
	if r := e.chat(backupModel); r.Status != 200 {
		t.Fatalf("second chat: %d %s", r.Status, r.Body)
	}
	if !listening() {
		t.Errorf("after a management change, the backend left the overridden port %d", port)
	}
	if show := e.cli(nil, "config", "show"); strings.Contains(show.stdout, fmt.Sprintf("port = %d\n", port)) {
		t.Errorf("the override was saved: %s", show.stdout)
	}
}
