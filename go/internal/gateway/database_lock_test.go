package gateway

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// A store's shared lock is how another process knows it's attached. Closing any descriptor on the database
// ends all of this process's locks on it, whatever opened it, so nothing a route does may open that file
// outside the store. These ask from another process, because a process can't see its own locks.

// lockAskerEnv names the database when this test binary runs as that other process.
const lockAskerEnv = "LOCALBASE_TEST_LOCK_ASKER"

// answerLockAsks is the other process. TestMain sends it here before it builds anything.
func answerLockAsks(path string) {
	lines := bufio.NewScanner(os.Stdin)
	for lines.Scan() {
		fmt.Println(askForTheLock(path))
	}
}

// askForTheLock asks for what a restore asks for.
func askForTheLock(path string) string {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_pragma=busy_timeout(0)")
	if err != nil {
		return "failed: " + err.Error()
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA locking_mode=EXCLUSIVE`); err != nil {
		return "failed: " + err.Error()
	}
	if _, err := db.Exec(`BEGIN EXCLUSIVE`); err != nil {
		if strings.Contains(err.Error(), "locked") || strings.Contains(err.Error(), "busy") {
			return "refused"
		}
		return "failed: " + err.Error()
	}
	return "granted"
}

type lockAsker struct {
	in  io.WriteCloser
	out *bufio.Reader
}

func newLockAsker(t *testing.T, path string) *lockAsker {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), lockAskerEnv+"="+path)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = in.Close() // it ends when its input does
		_ = cmd.Wait()
	})
	return &lockAsker{in: in, out: bufio.NewReader(out)}
}

// mustBeRefused fails if another process could now take the database, and says what had just happened.
func (a *lockAsker) mustBeRefused(t *testing.T, after string) {
	t.Helper()
	if _, err := fmt.Fprintln(a.in, "ask"); err != nil {
		t.Fatalf("the other process, after %s: %v", after, err)
	}
	line, err := a.out.ReadString('\n')
	if err != nil {
		t.Fatalf("the other process, after %s: %v", after, err)
	}
	if got := strings.TrimSpace(line); got != "refused" {
		t.Fatalf("after %s, another process asking for the database's lock got: %s", after, got)
	}
}

// The control: "refused" above only means something if it can be granted.
func TestTheLockIsGrantedWithNothingAttached(t *testing.T) {
	path := filepath.Join(scratchDir(t), "local-base.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if got := askForTheLock(path); got != "granted" {
		t.Errorf("with nothing attached: %s", got)
	}
}

// Every route, each answered and then asked about, so a failure names the route. This walk is shallow:
// TestOpenAPIResponses, which asks once at its end, is the deep one.
func TestNoRouteCostsTheGatewayItsLock(t *testing.T) {
	t.Run("the API", func(t *testing.T) {
		// Nothing here reaches the network: the Hub is the recorded responses.
		t.Setenv("HF_ENDPOINT", openAPIHub(t).URL)
		t.Setenv("HF_TOKEN", "")
		ts := openAPIGateway(t)
		s := runServer(ts)
		_, key, err := s.Store.CreateAPIKey(t.Context(), "all", auth.Permissions)
		if err != nil {
			t.Fatal(err)
		}
		asker := newLockAsker(t, s.Root.DB())
		asker.mustBeRefused(t, "opening")
		answered := 0
		for path := range matrixPaths(gatewayPatterns(t)) {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second) // some routes stream
				req, err := http.NewRequestWithContext(ctx, method, ts.URL+path, strings.NewReader("{}"))
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+key)
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body) // what it says doesn't matter here
					resp.Body.Close()
					// A handler ran: not refused at the door, and not a route or method that isn't there.
					switch resp.StatusCode {
					case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed:
					default:
						answered++
					}
				}
				cancel()
				asker.mustBeRefused(t, method+" "+path)
			}
		}
		// 45 handlers answered when this was written. Far fewer means the walk stops at the door.
		if answered < 40 {
			t.Errorf("only %d handlers answered, so this walked very little", answered)
		}
	})
	t.Run("the pages", func(t *testing.T) {
		g := newMatrixGateway(t, nil)
		// Signed in, as the owner at this computer, so the pages answer rather than ask who it is.
		cookie := SessionCookie(t, LocalClient{T: t, Base: g.ts.URL}, g.srv.Store)
		signedIn := LocalUI(cookie, g.ts.URL)
		asker := newLockAsker(t, g.srv.Root.DB())
		asker.mustBeRefused(t, "opening")
		answered := 0
		for path := range matrixPaths(uiPatterns(t)) {
			if r := g.send(t, http.MethodGet, path, "", signedIn); r.status == http.StatusOK {
				answered++
			}
			asker.mustBeRefused(t, "GET "+path)
		}
		// 26 pages answered when this was written. Far fewer means the walk stops at sign-in.
		if answered < 20 {
			t.Errorf("only %d pages answered, so this walked very little", answered)
		}
	})
}

// Asking to import the database is refused before it's opened, because opening it is what would cost the
// lock. A refusal after a look would pass every test that only reads the answer.
func TestImportingTheDatabaseLeavesItsLock(t *testing.T) {
	t.Setenv("HF_ENDPOINT", openAPIHub(t).URL)
	t.Setenv("HF_TOKEN", "")
	ts := openAPIGateway(t)
	s := runServer(ts)
	_, key, err := s.Store.CreateAPIKey(t.Context(), "all", auth.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetSetting(t.Context(), "database-lock-test", "so there's a -wal"); err != nil {
		t.Fatal(err)
	}
	db := s.Root.DB()
	link := filepath.Join(scratchDir(t), "m.gguf")
	if err := os.Symlink(db, link); err != nil {
		t.Fatal(err)
	}
	asker := newLockAsker(t, db)
	asker.mustBeRefused(t, "opening")
	for _, source := range []string{db, db + "-wal", db + "-shm", link} {
		if _, err := os.Stat(source); err != nil {
			t.Fatalf("this would test nothing: %v", err)
		}
		for _, action := range []string{"resolve", "start"} {
			body, err := json.Marshal(map[string]any{"action": action, "source": source, "task": "chat", "id": "x"})
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/_localbase/model-imports", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			out, _ := io.ReadAll(resp.Body) // a short read shows in the checks below
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest || !bytes.Contains(out, []byte("import_unresolved")) ||
				!bytes.Contains(out, []byte("own database")) {
				t.Errorf("%s %s: %d %s", action, source, resp.StatusCode, firstBytes(out))
			}
			asker.mustBeRefused(t, action+" of "+source)
		}
	}
}

// The name in a refusal is the file's, never its folder, and it can hold anything a file name can, so it goes
// through what every import error goes through. Here that's the API's: escape sequences removed, then JSON.
func TestARefusalNamesOnlyTheFileAndEscapesIt(t *testing.T) {
	t.Setenv("HF_ENDPOINT", openAPIHub(t).URL)
	t.Setenv("HF_TOKEN", "")
	ts := openAPIGateway(t)
	s := runServer(ts)
	_, key, err := s.Store.CreateAPIKey(t.Context(), "all", auth.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	folder := scratchDir(t)
	link := filepath.Join(folder, "m\x1b[2Jx\n.gguf")
	if err := os.Symlink(s.Root.DB(), link); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"action": "resolve", "source": link, "task": "chat", "id": "x"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/_localbase/model-imports", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := "mx\n.gguf is part of LocalBase's own database, so it can't be imported."
	if resp.StatusCode != http.StatusBadRequest || got.Error.Code != "import_unresolved" || got.Error.Message != want {
		t.Errorf("%d %q: %q, want %q", resp.StatusCode, got.Error.Code, got.Error.Message, want)
	}
	if strings.Contains(got.Error.Message, folder) {
		t.Errorf("the refusal shows the folder: %q", got.Error.Message)
	}
}
