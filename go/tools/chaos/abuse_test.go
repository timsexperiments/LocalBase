package chaos

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/tools/lab"
)

// rawRequest writes a request by hand, so a test controls exactly what the
// gateway sees and when.
func rawHeader(g *lab.Gateway, path, key string, contentLength int) string {
	return fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n",
		path, g.Addr(), key, contentLength)
}

func dial(t *testing.T, g *lab.Gateway) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", g.Addr(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// A body far past the limit is refused without being held in memory, declared or
// not, and the gateway's footprint does not follow it.
func TestHugeBodies(t *testing.T) {
	ctx, g, c := start(t, lab.Options{})
	first := settledStats(t, ctx, g, c)
	const size = 200 << 20
	huge := func(chunked bool) lab.Call {
		var body io.Reader = io.LimitReader(zeros{}, size)
		req, _ := http.NewRequestWithContext(ctx, "POST", g.Base+chatPath, body)
		req.Header.Set("Authorization", "Bearer "+g.Keys.User)
		req.Header.Set("Content-Type", "application/json")
		if !chunked {
			req.ContentLength = size
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return lab.Call{Err: err}
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return lab.Call{Status: resp.StatusCode, Body: b, Code: lab.ErrorCode(b)}
	}
	for _, chunked := range []bool{false, true} {
		r := huge(chunked)
		// A client may see the connection close while it is still sending; what
		// must not happen is a success or a server error.
		if r.Err == nil && r.Status != 413 {
			t.Errorf("a 200 MiB body (chunked %v): %d %.200s", chunked, r.Status, r.Body)
		}
	}
	last := settledStats(t, ctx, g, c)
	// A race-detector gateway's memory is several times larger.
	budget := int64(128 << 20)
	if os.Getenv("LOCALBASE_CHAOS_RACE") != "" {
		budget *= 4
	}
	t.Logf("RSS across two 200 MiB bodies: %d > %d MiB (budget +%d)", first.RSS>>20, last.RSS>>20, budget>>20)
	if last.RSS-first.RSS > budget {
		t.Errorf("RSS followed a refused body: %d > %d MiB", first.RSS>>20, last.RSS>>20)
	}
	if r := c.Do(ctx, "GET", "/health", "", "", nil); r.Status != 200 {
		t.Errorf("health after huge bodies: %d", r.Status)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// A slow-body client holds a connection for as long as it likes, but many of them
// cost the others nothing, and leaving frees everything.
func TestSlowBodies(t *testing.T) {
	ctx, g, c := start(t, lab.Options{})
	first := settledStats(t, ctx, g, c)
	const clients = 100
	conns := make([]net.Conn, 0, clients)
	for range clients {
		conn := dial(t, g)
		if _, err := io.WriteString(conn, rawHeader(g, chatPath, g.Keys.User, 1<<20)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(conn, `{"model":"`); err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
	}
	waitFor(t, "the gateway to hold them", 10*time.Second, func() bool {
		s, err := g.Stats(ctx)
		return err == nil && s.Goroutines >= first.Goroutines+clients
	})
	began := time.Now()
	if r := c.Do(ctx, "GET", "/health", "", "", nil); r.Status != 200 {
		t.Errorf("health among slow bodies: %d", r.Status)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("health took %v among slow bodies", took)
	}
	mustChat(t, ctx, c, g, lab.ChatSmall)
	for _, conn := range conns {
		conn.Close()
	}
	waitFor(t, "the slow clients' goroutines to go", 15*time.Second, func() bool {
		s, err := g.Stats(ctx)
		return err == nil && s.Goroutines <= first.Goroutines+20
	})
	checkGrowth(t, first, settledStats(t, ctx, g, c), 12, 6)
}

// A SIGTERM does not wait on slow clients past the drain's bound.
func TestStopWithSlowBodies(t *testing.T) {
	_, g, _ := start(t, lab.Options{})
	for range 20 {
		conn := dial(t, g)
		io.WriteString(conn, rawHeader(g, chatPath, g.Keys.User, 1<<20))
	}
	time.Sleep(200 * time.Millisecond)
	if err := g.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-g.Done():
	case <-time.After(25 * time.Second):
		t.Error("a SIGTERM waited over 25 s on clients that never finish their bodies")
	}
}

// A body that ends early, a body that is not JSON at any depth, and headers past
// the limit: each gets an answer or a closed connection, never a panic.
func TestMalformedRequests(t *testing.T) {
	ctx, g, c := start(t, lab.Options{})
	t.Run("a body shorter than declared", func(t *testing.T) {
		conn := dial(t, g)
		io.WriteString(conn, rawHeader(g, chatPath, g.Keys.User, 1<<20)+`{"model":`)
		if tcp, ok := conn.(*net.TCPConn); ok {
			tcp.CloseWrite()
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		b, _ := io.ReadAll(conn)
		if s := string(b); s != "" && !strings.HasPrefix(s, "HTTP/1.1 4") {
			t.Errorf("answered %.200q", s)
		}
	})
	for _, path := range []string{chatPath, "/v1/embeddings", "/v1/systemone", "/v1/images/generations", "/v1/audio/speech", "/_localbase/runs/v1/chat/completions"} {
		t.Run("deep nesting at "+path, func(t *testing.T) {
			deep := bytes.Repeat([]byte("["), 4<<20)
			r := c.Do(ctx, "POST", path, g.Keys.User, "application/json", deep)
			if r.Err != nil || r.Status != 400 {
				t.Errorf("%d %v %.200s", r.Status, r.Err, r.Body)
			}
			nested := `{"model":"` + lab.ChatSmall + `","messages":` + strings.Repeat(`{"a":`, 200000) + "1" + strings.Repeat("}", 200000) + "}"
			r = c.Do(ctx, "POST", path, g.Keys.User, "application/json", []byte(nested))
			if r.Err != nil || r.Status != 400 {
				t.Errorf("deep objects: %d %v %.200s", r.Status, r.Err, r.Body)
			}
		})
	}
	t.Run("invalid text", func(t *testing.T) {
		r := c.Do(ctx, "POST", chatPath, g.Keys.User, "application/json", []byte("{\"model\":\"\xff\xfe\",\"messages\":[]}"))
		if r.Err != nil || r.Status >= 500 {
			t.Errorf("%d %v %.200s", r.Status, r.Err, r.Body)
		}
	})
	t.Run("headers past the limit", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, "GET", g.Base+"/health", nil)
		req.Header.Set("X-Big", strings.Repeat("a", 2<<20))
		resp, err := c.HTTP.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
				t.Errorf("%d", resp.StatusCode)
			}
		}
	})
	if r := c.Do(ctx, "GET", "/health", "", "", nil); r.Status != 200 {
		t.Errorf("health after malformed requests: %d", r.Status)
	}
	mustChat(t, ctx, c, g, lab.ChatSmall)
}

// --- many keys ---------------------------------------------------------------

func percentiles(d []time.Duration) (p50, p99 time.Duration) {
	s := lab.Sorted(d)
	return lab.Percentile(s, 50), lab.Percentile(s, 99)
}

// A thousand API keys, created through the API while requests run: creation stays
// reasonable, a request's authentication stays cheap, the list stays usable, and
// a revoked key stops at once.
func TestThousandKeys(t *testing.T) {
	ctx, g, c := start(t, lab.Options{})
	sample := func(key string, n int) (p50, p99 time.Duration) {
		t.Helper()
		var d []time.Duration
		for range n {
			r := c.Do(ctx, "GET", "/v1/models", key, "", nil)
			if r.Err != nil || r.Status != 200 {
				t.Fatalf("models: %d %v %.200s", r.Status, r.Err, r.Body)
			}
			d = append(d, r.Dur)
		}
		return percentiles(d)
	}
	base50, base99 := sample(g.Keys.User, 200)
	const keys = 1000
	var created []time.Duration
	var lastKey string
	var mu sync.Mutex
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // reads running through the writes
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if r := c.Do(ctx, "GET", "/v1/models", g.Keys.User, "", nil); r.Err != nil || r.Status != 200 {
				mu.Lock()
				t.Errorf("a read during key creation: %d %v %.200s", r.Status, r.Err, r.Body)
				mu.Unlock()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	for i := range keys {
		r := c.PostJSON(ctx, "/_localbase/api-keys", g.Keys.Admin, map[string]any{"action": "create", "name": fmt.Sprintf("chaos-%d", i), "scopes": []string{"models:read"}})
		if r.Err != nil || r.Status >= 300 {
			close(stop)
			wg.Wait()
			t.Fatalf("creating key %d: %d %v %.300s", i, r.Status, r.Err, r.Body)
		}
		created = append(created, r.Dur)
		if i == keys-1 {
			var out struct {
				Secret string `json:"secret"`
			}
			json.Unmarshal(r.Body, &out)
			lastKey = out.Secret
		}
	}
	close(stop)
	wg.Wait()
	full50, full99 := sample(g.Keys.User, 200)
	createdP50, createdP99 := percentiles(created)
	began := time.Now()
	list := c.Do(ctx, "GET", "/_localbase/api-keys", g.Keys.Admin, "", nil)
	listTook := time.Since(began)
	if list.Err != nil || list.Status != 200 {
		t.Fatalf("the key list: %d %v", list.Status, list.Err)
	}
	t.Logf("auth with 6 keys p50 %v p99 %v; with %d keys p50 %v p99 %v; creation p50 %v p99 %v; list %v, %d KiB",
		base50, base99, keys+6, full50, full99, createdP50, createdP99, listTook, len(list.Body)>>10)
	if full99 > 250*time.Millisecond {
		t.Errorf("a request's p99 is %v with %d keys", full99, keys)
	}
	if listTook > 2*time.Second {
		t.Errorf("listing %d keys took %v", keys, listTook)
	}
	if lastKey == "" {
		t.Fatal("no key in the creation response; cannot check revocation")
	}
	if r := c.Do(ctx, "GET", "/v1/models", lastKey, "", nil); r.Status != 200 {
		t.Errorf("the newest of %d keys: %d", keys, r.Status)
	}
	var listed struct {
		Keys []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(list.Body, &listed); err != nil {
		t.Fatalf("the key list: %v %.200s", err, list.Body)
	}
	id := ""
	for _, k := range listed.Keys {
		if k.Name == fmt.Sprintf("chaos-%d", keys-1) {
			id = k.ID
		}
	}
	if id == "" {
		t.Fatalf("key chaos-%d is not in a list of %d", keys-1, len(listed.Keys))
	}
	revoke := c.PostJSON(ctx, "/_localbase/api-keys", g.Keys.Admin, map[string]any{"action": "revoke", "keyId": id})
	if revoke.Status >= 300 {
		t.Fatalf("revoking: %d %.300s", revoke.Status, revoke.Body)
	}
	if r := c.Do(ctx, "GET", "/v1/models", lastKey, "", nil); r.Status != 401 {
		t.Errorf("a revoked key: %d", r.Status)
	}
}
