package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
)

func TestUpdateRoute(t *testing.T) {
	ts := openAPIGateway(t)
	s := runServer(ts)
	_, reader := oaKey(t, s, "reader", "models:read")
	_, inference := oaKey(t, s, "inference", "inference:chat")
	_, system := oaKey(t, s, "system", "system:manage", "models:read")
	_, access := oaKey(t, s, "access", "access:manage")
	read := func(key string) (int, upgrade.State) {
		t.Helper()
		status, _, b, _ := (oaClient{t, ts.URL, key}).do("GET", "/_localbase/update")
		var st upgrade.State
		json.Unmarshal(b, &st)
		return status, st
	}
	// Off by default, and reading asks nobody.
	status, st := read(reader)
	if status != 200 || st.Enabled || st.UpdateAvailable || st.Latest != nil || st.Current == "" || st.Object != "localbase.update" {
		t.Errorf("%d %+v", status, st)
	}
	if got, _ := read(inference); got != 403 {
		t.Errorf("without models:read: %d", got)
	}
	if got, _ := read(""); got != 401 {
		t.Errorf("no key: %d", got)
	}
	set := func(key, body string) int {
		t.Helper()
		status, _, _, _ := (oaClient{t, ts.URL, key}).doBody(body, "POST", "/_localbase/update")
		return status
	}
	on, off := `{"action":"set-check","enabled":true}`, `{"action":"set-check","enabled":false}`
	if got := set(reader, on); got != 403 {
		t.Errorf("models:read: %d", got)
	}
	if got := set("", on); got != 401 {
		t.Errorf("no key: %d", got)
	}
	if got := set(system, on); got != 200 {
		t.Errorf("system:manage: %d", got)
	}
	if _, st := read(reader); !st.Enabled {
		t.Errorf("not on: %+v", st)
	}
	if got := set(access, off); got != 200 {
		t.Errorf("access:manage: %d", got)
	}
	for _, bad := range []string{`{}`, `{"action":"install"}`, `{"action":"set-check"}`, `{"action":"set-check","enabled":"yes"}`, `{"action":"set-check","enabled":true,"x":1}`, `nope`} {
		if got := set(system, bad); got != 400 {
			t.Errorf("%s: %d", bad, got)
		}
	}
	if got := set(system, `{"action":"set-check","enabled":true,"pad":"`+string(bytes.Repeat([]byte("x"), 5000))+`"}`); got != 413 {
		t.Errorf("too large: %d", got)
	}
	person := &auth.Principal{Kind: "browser-session", ID: "browser:owner", Permissions: []string{"access:manage"}}
	if got := s.Call(t.Context(), person, "POST", "/_localbase/update", "application/json", []byte(on)).Status; got != 200 {
		t.Errorf("the owner's role through the page: %d", got)
	}
	if got := s.Call(t.Context(), person, "GET", "/_localbase/update", "", nil).Status; got != http.StatusForbidden {
		t.Errorf("access:manage alone reading: %d", got)
	}
}

// The instance token works here as on usage: writes only, and the same rule (DESIGN §32.4).
func TestUpdateInstanceToken(t *testing.T) {
	ts := openAPIGateway(t)
	s := runServer(ts)
	release, err := s.cfg.Root.Lock("127.0.0.1", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	token := s.cfg.Root.Instance().InstanceToken
	c := oaClient{t, ts.URL, ""}
	host := "X-Localbase-Instance-Token"
	if status, _, _, _ := c.doBody(`{"action":"set-check","enabled":true}`, "POST", "/_localbase/update", host, token); status != 200 {
		t.Errorf("the token: %d", status)
	}
	if status, _, _, _ := c.doBody(`{"action":"set-check","enabled":true}`, "POST", "/_localbase/update", host, "wrong"); status != 401 {
		t.Errorf("a wrong token: %d", status)
	}
	if status, _, _, _ := c.do("GET", "/_localbase/update", host, token); status != 401 {
		t.Errorf("reading with the token: %d", status)
	}
}

func TestUpdateBrowserRoute(t *testing.T) {
	if !allowedUIRoute("GET", "/_localbase/update") || !allowedUIRoute("POST", "/_localbase/update") || allowedUIRoute("DELETE", "/_localbase/update") {
		t.Error("the browser's route")
	}
}
