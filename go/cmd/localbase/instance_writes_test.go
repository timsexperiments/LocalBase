package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
)

// writeSitesEnv is a configured root, open, whose owner record names a gateway served by handler,
// which also answers the identity check before each write. It returns the root's directory too.
func writeSitesEnv(t *testing.T, handler http.HandlerFunc) (*env, string) {
	t.Helper()
	t.Setenv("LOCALBASE_ROOT", "")
	dir := t.TempDir() + "/root"
	if code, out := runApp(t, "--root", dir, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	e, err := readEnv(&cli.Ctx{Global: cli.Global{Root: dir}}, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_localbase/instance" && r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]string{"instanceId": "id", "rootHash": "hash"})
			return
		}
		handler(w, r)
	}))
	t.Cleanup(gw.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(gw.URL, "http://"))
	writeOwner(t, e, host, atoi(t, port), "id", "hash")
	return e, dir
}

// sent is what a fake gateway got: the method, route, token and JSON body of each write.
type sent struct {
	mu  sync.Mutex
	got []string
}

func (s *sent) add(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, r.Method+" "+r.URL.Path+" token="+r.Header.Get("X-Localbase-Instance-Token")+
		" type="+r.Header.Get("Content-Type")+" "+strings.TrimSpace(string(b)))
}

func (s *sent) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

// models cancel posts its cancel to the proven gateway, with the token, and answers with the
// gateway's operation; a refusal the gateway explains is the gateway's own message.
func TestModelsCancelAsksTheProvenGateway(t *testing.T) {
	var s sent
	refuse := false
	_, dir := writeSitesEnv(t, func(w http.ResponseWriter, r *http.Request) {
		s.add(r)
		if refuse {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":{"message":"Nothing is installing that model."}}`))
			return
		}
		w.Write([]byte(`{"modelId":"m","state":"cancelled","detail":"Stopped."}`))
	})
	if code, out := runApp(t, "--root", dir, "models", "cancel", "m"); code != 0 || !strings.Contains(out, "Stopped.") {
		t.Errorf("models cancel: exit %d, %s", code, out)
	}
	refuse = true
	if code, out := runApp(t, "--root", dir, "models", "cancel", "m"); code == 0 || !strings.Contains(out, "Nothing is installing that model.") {
		t.Errorf("models cancel, refused: exit %d, %s", code, out)
	}
	want := `POST /_localbase/model-management token=the-token type=application/json {"action":"cancel","modelId":"m"}`
	if got := s.all(); len(got) != 2 || got[0] != want || got[1] != want {
		t.Errorf("the gateway got:\n%s\nwant twice:\n%s", strings.Join(got, "\n"), want)
	}
}

// A write through the gateway posts the body to the proven gateway, with the token, and answers
// with what the gateway did, marked as through the gateway.
func TestAWriteGoesThroughTheProvenGateway(t *testing.T) {
	var s sent
	e, _ := writeSitesEnv(t, func(w http.ResponseWriter, r *http.Request) {
		s.add(r)
		w.Write([]byte(`{"object":"usage.settings","recording":false}`))
	})
	wrote := false
	out, err := writeThroughGateway(e, "/_localbase/usage", map[string]any{"action": "pause"}, func() (map[string]any, error) {
		wrote = true
		return map[string]any{}, nil
	})
	if err != nil || wrote || out["via"] != "gateway" || out["recording"] != false || out["object"] != nil {
		t.Errorf("writeThroughGateway: %v %v, wrote the database directly %v", out, err, wrote)
	}
	want := `POST /_localbase/usage token=the-token type=application/json {"action":"pause"}`
	if got := s.all(); len(got) != 1 || got[0] != want {
		t.Errorf("the gateway got:\n%s\nwant:\n%s", strings.Join(got, "\n"), want)
	}
}

// Neither write follows a redirect. The proven gateway sends each write elsewhere, to a server that
// accepts it as the gateway would: following it would hand that server the token and the change.
func TestTheWritesFollowNoRedirect(t *testing.T) {
	var elsewhere sent
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.add(r)
		w.Write([]byte(`{"modelId":"m","state":"cancelled","recording":false}`))
	}))
	defer other.Close()
	e, dir := writeSitesEnv(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	})
	if code, out := runApp(t, "--root", dir, "models", "cancel", "m"); code == 0 || !strings.Contains(out, "(HTTP 307)") {
		t.Errorf("models cancel through a redirect: exit %d, %s", code, out)
	}
	if out, err := writeThroughGateway(e, "/_localbase/usage", map[string]any{"action": "pause"}, func() (map[string]any, error) {
		return map[string]any{}, nil
	}); err == nil || !strings.Contains(err.Error(), "(HTTP 307)") {
		t.Errorf("a write through a redirect: %v %v", out, err)
	}
	if got := elsewhere.all(); len(got) > 0 {
		t.Errorf("a redirect was followed:\n%s", strings.Join(got, "\n"))
	}
}
