package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// limitsGateway is a gateway with authentication on, the fake LLM and short limits.
type limitsGateway struct {
	ts       *httptest.Server
	srv      *Server
	store    *store.Store
	key      string // inference:chat
	all      string // every permission
	noScopes string
	bodyRead *atomic.Int64
}

type byteCountBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (b byteCountBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, err
}

func newLimitsGateway(t *testing.T, limits httpx.Limits, configure func(*Config)) *limitsGateway {
	t.Helper()
	dir := scratchDir(t)
	r, err := root.Init(filepath.Join(dir, "root"))
	if err != nil {
		t.Fatal(err)
	}
	modelsDir := r.ModelsDir("llm")
	os.MkdirAll(modelsDir, 0o700)
	cat, err := catalog.LoadWithFormer()
	if err != nil {
		t.Fatal(err)
	}
	installSparse(t, modelsDir, cat.ByID(gatewaytest.ChatModel))
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_, key, err := st.CreateAPIKey(context.Background(), "chat", []string{"inference:chat"})
	if err != nil {
		t.Fatal(err)
	}
	_, noScopes, err := st.CreateAPIKey(context.Background(), "none", []string{})
	if err != nil {
		t.Fatal(err)
	}
	_, all, err := st.CreateAPIKey(context.Background(), "all", auth.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &store.Config{Host: "127.0.0.1", Port: freePort(t), CtxSize: 8192, CtxMode: "auto", Parallel: "1",
		Active: map[string]string{"llm": gatewaytest.ChatModel}, Selected: map[string][]string{"llm": {gatewaytest.ChatModel}}}
	cfg.Root, cfg.GatewayHost, cfg.SttHost = r.Dir, "127.0.0.1", "127.0.0.1"
	if err := st.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	mem := memory.NewController(unsampled{host{}}, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	llm := runtime.NewLLM(runtime.LLMDeps{Root: r.Dir, ModelsDir: modelsDir, Store: st, Catalog: cat, Memory: mem,
		Log: log, MemoryGB: 64, Binary: fakeBinary, QueueCapacity: 4, QueueTimeout: 5 * time.Second}, cfg)
	t.Cleanup(llm.Close)
	conf := Config{Store: st, Catalog: cat, Auth: &auth.Resolver{Store: st, Mode: "either"}, LLM: llm, Log: log,
		MemoryGB: 64, Root: r, Memory: mem, Limits: limits}
	if configure != nil {
		configure(&conf)
	}
	srv := newGateway(t, conf)
	bodyRead := &atomic.Int64{}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Body != nil && req.Body != http.NoBody {
			req.Body = byteCountBody{req.Body, bodyRead}
		}
		srv.ServeHTTP(w, req)
	}))
	limits.Apply(ts.Config)
	ts.Start()
	t.Cleanup(ts.Close)
	return &limitsGateway{ts: ts, srv: srv, store: st, key: key, all: all, noScopes: noScopes, bodyRead: bodyRead}
}

func (g *limitsGateway) dial(t *testing.T) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", g.ts.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// served reports how a connection ended up within wait: the status it was answered
// with (0 for none) and whether the server closed it.
func served(c net.Conn, wait time.Duration) (status int, closed bool) {
	c.SetReadDeadline(time.Now().Add(wait))
	r := bufio.NewReader(c)
	if line, err := r.ReadString('\n'); err == nil {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			status, _ = strconv.Atoi(fields[1])
		}
	} else {
		ne, isNet := err.(net.Error)
		return 0, !(isNet && ne.Timeout())
	}
	buf := make([]byte, 4096)
	for {
		if _, err := r.Read(buf); err != nil {
			ne, isNet := err.(net.Error)
			return status, !(isNet && ne.Timeout())
		}
	}
}

// A client that declares a body and never sends it holds nothing for long, and is
// answered without waiting for its deadline.
func TestUnsentBodyCannotHoldAConnection(t *testing.T) {
	limits := httpx.Limits{PreAuthBody: 300 * time.Millisecond, RefusedDrain: 100 * time.Millisecond, ReadHeader: 300 * time.Millisecond, Idle: 300 * time.Millisecond}
	g := newLimitsGateway(t, limits, nil)
	for _, c := range []struct {
		name, request string
		status        int
		also          int // another answer that is fine
	}{
		{"guarded route, large body", "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nContent-Length: 1000000\r\n\r\n", 401, 0},
		{"guarded route, small body", "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n", 401, 0},
		{"public route", "POST /health HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n", 405, 0},
		{"browser layer", "POST /app/logout HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n", 401, 0},
		{"admin route", "POST /_localbase/api-keys HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n", 401, 408},
		{"chunked body", "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n", 401, 0},
		{"partial headers", "GET /health HTTP/1.1\r\nHost: x\r\n", 0, 0},
		{"idle after a request", "GET /health HTTP/1.1\r\nHost: x\r\n\r\n", 200, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn := g.dial(t)
			if _, err := conn.Write([]byte(c.request)); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			status, closed := served(conn, 5*time.Second)
			if (status != c.status && (c.also == 0 || status != c.also)) || !closed {
				t.Errorf("status %d, closed %v after %v; want %d, closed", status, closed, time.Since(start).Round(time.Millisecond), c.status)
			}
		})
	}
}

// A refusal does not wait out the body deadline: it answers at once when it has
// not read the body, or within the drain's own short window.
func TestRefusalsAnswerPromptly(t *testing.T) {
	limits := httpx.Limits{PreAuthBody: 10 * time.Second, RefusedDrain: 100 * time.Millisecond}
	g := newLimitsGateway(t, limits, nil)
	for _, request := range []string{
		"POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n",
		"POST /app/logout HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n",
	} {
		conn := g.dial(t)
		conn.Write([]byte(request))
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil || !strings.Contains(line, "401") {
			t.Errorf("%q: %q %v, want a 401 well before the body deadline", request, line, err)
		}
	}
}

// A refused upload is read only up to its own small cap, however much is coming.
func TestRefusedUploadIsNotReadWhole(t *testing.T) {
	g := newLimitsGateway(t, httpx.Limits{RefusedDrainBytes: 64 << 10, RefusedDrain: 500 * time.Millisecond}, nil)
	for _, c := range []struct {
		name, header string
		status       int
	}{
		{"no credential", "", 401},
		{"no permission", "Authorization: Bearer " + g.noScopes + "\r\n", 403},
	} {
		t.Run(c.name, func(t *testing.T) {
			g.bodyRead.Store(0)
			conn := g.dial(t)
			const size = 64 << 20
			fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\n%sContent-Length: %d\r\n\r\n", c.header, size)
			go func() { // keep sending, as a client uploading a file does
				chunk := make([]byte, 1<<16)
				for sent := 0; sent < size; sent += len(chunk) {
					conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
					if _, err := conn.Write(chunk); err != nil {
						return
					}
				}
			}()
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != c.status || !resp.Close {
				t.Errorf("status %d, close %v; want %d and close", resp.StatusCode, resp.Close, c.status)
			}
			// What the server's own buffers took is small; the point is it is not 32 MiB.
			if n := g.bodyRead.Load(); n > 2<<20 {
				t.Errorf("read %d bytes of a refused body", n)
			}
		})
	}
}

// A small refused body is read whole, so the client keeps its connection.
func TestSmallRefusedBodyKeepsTheConnection(t *testing.T) {
	g := newLimitsGateway(t, httpx.Limits{}, nil)
	conn := g.dial(t)
	r := bufio.NewReader(conn)
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	for i := range 2 {
		fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\n\r\nhello")
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 401 || resp.Close {
			t.Fatalf("request %d: status %d, close %v", i, resp.StatusCode, resp.Close)
		}
	}
}

// A client waiting for permission to send is refused without being asked to send.
func TestRefusalDoesNotAskForTheBody(t *testing.T) {
	g := newLimitsGateway(t, httpx.Limits{}, nil)
	conn := g.dial(t)
	fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nExpect: 100-continue\r\nContent-Length: 1000000\r\n\r\n")
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("first response %d, want 401 with no 100 Continue before it", resp.StatusCode)
	}
}

// paddedChat is a valid request padded with whitespace to size bytes.
func paddedChat(content string, size int) []byte {
	b, _ := json.Marshal(map[string]any{"model": gatewaytest.ChatModel, "messages": []map[string]string{{"role": "user", "content": content}}})
	if len(b) < size {
		b = append(b, bytes.Repeat([]byte(" "), size-len(b))...)
	}
	return b
}

// throttled sends a body in pieces over the given time, like a phone on a slow link.
func throttled(body []byte, pieces int, gap time.Duration) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		size := len(body)/pieces + 1
		for len(body) > 0 {
			n := min(size, len(body))
			if _, err := pw.Write(body[:n]); err != nil {
				return
			}
			body = body[n:]
			time.Sleep(gap)
		}
	}()
	return pr
}

// An authenticated upload may take far longer than the pre-authentication deadline,
// as long as it keeps moving: a real one is 25 MiB at 0.5 Mbit/s, scaled here.
func TestSlowAuthenticatedUploadSurvives(t *testing.T) {
	g := newLimitsGateway(t, httpx.Limits{PreAuthBody: 150 * time.Millisecond, UploadStall: 500 * time.Millisecond}, nil)
	body := paddedChat("fake:reply received", 4<<20)
	req, _ := http.NewRequest(http.MethodPost, g.ts.URL+"/v1/chat/completions", throttled(body, 20, 60*time.Millisecond))
	req.Header.Set("Authorization", "Bearer "+g.key)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(out), "received") {
		t.Fatalf("after %v: %d %.200s", time.Since(start).Round(time.Millisecond), resp.StatusCode, out)
	}
	if elapsed := time.Since(start); elapsed < 5*150*time.Millisecond {
		t.Fatalf("the upload took only %v, so it did not outlast the deadline", elapsed)
	}
}

// An upload that stops moving is cut, and one that never ends is cut at the total.
func TestStalledUploadIsCut(t *testing.T) {
	g := newLimitsGateway(t, httpx.Limits{PreAuthBody: 150 * time.Millisecond, UploadStall: 300 * time.Millisecond, RefusedDrain: 50 * time.Millisecond}, nil)
	conn := g.dial(t)
	fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: 100000\r\n\r\n{", g.key)
	start := time.Now()
	status, closed := served(conn, 5*time.Second)
	elapsed := time.Since(start)
	if status != 408 || !closed {
		t.Errorf("status %d, closed %v; want 408, closed", status, closed)
	}
	if elapsed < 250*time.Millisecond {
		t.Errorf("cut after %v, before the stall limit", elapsed)
	}
}

// The 408 is the same on every route that reads a body, whatever the handler made
// of the failed read, and it says what happened.
func TestStalledBodyIsATimeoutEverywhere(t *testing.T) {
	logs := &gatewaytest.SyncBuffer{}
	limits := httpx.Limits{PreAuthBody: 300 * time.Millisecond, UploadStall: 200 * time.Millisecond, RefusedDrain: 50 * time.Millisecond}
	g := newLimitsGateway(t, limits, func(c *Config) { c.Log = slog.New(slog.NewJSONHandler(logs, nil)) })
	for _, path := range []string{"/v1/chat/completions", "/v1/embeddings", "/v1/audio/transcriptions"} {
		key := g.key
		if path != "/v1/chat/completions" {
			key = g.all
		}
		conn := g.dial(t)
		fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: 100000\r\n\r\n{", path, key)
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var out struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 408 || out.Error.Code != "request_timeout" || !resp.Close {
			t.Errorf("%s: %d %+v %v, close %v", path, resp.StatusCode, out, err, resp.Close)
		}
	}
	if !gatewaytest.WaitFor(2*time.Second, func() bool { return strings.Contains(logs.String(), `"error_code":"request_timeout"`) }) {
		t.Errorf("no request_timeout in the request log:\n%s", logs.String())
	}
}

// Only a deadline is a timeout: a client that goes away mid-upload stays a 499.
func TestClientThatLeavesIsNotATimeout(t *testing.T) {
	logs := &gatewaytest.SyncBuffer{}
	g := newLimitsGateway(t, httpx.Limits{}, func(c *Config) { c.Log = slog.New(slog.NewJSONHandler(logs, nil)) })
	conn := g.dial(t)
	fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: 100000\r\n\r\n{", g.key)
	time.Sleep(100 * time.Millisecond)
	conn.Close()
	if !gatewaytest.WaitFor(5*time.Second, func() bool { return strings.Contains(logs.String(), `"status":499`) }) {
		t.Errorf("want a 499 in the request log:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "request_timeout") {
		t.Errorf("a client that left was logged as a timeout:\n%s", logs.String())
	}
}

func TestUploadTotalIsBounded(t *testing.T) {
	g := newLimitsGateway(t, httpx.Limits{UploadStall: 300 * time.Millisecond, UploadTotal: 500 * time.Millisecond, RefusedDrain: 50 * time.Millisecond}, nil)
	conn := g.dial(t)
	fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: 100000\r\n\r\n", g.key)
	go func() {
		for range 200 { // keeps moving for far longer than the total
			conn.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err := conn.Write([]byte(" ")); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	start := time.Now()
	if status, _ := served(conn, 3*time.Second); status != 408 {
		t.Errorf("status %d after %v, want 408", status, time.Since(start).Round(time.Millisecond))
	}
}

// A long response outlives every body deadline: the deadline ends with the body.
func TestLongStreamSurvivesTheDeadlines(t *testing.T) {
	g := newLimitsGateway(t, httpx.Limits{PreAuthBody: 100 * time.Millisecond, UploadStall: 150 * time.Millisecond}, nil)
	body, _ := json.Marshal(map[string]any{"model": gatewaytest.ChatModel, "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "fake:slow 24"}}})
	req, _ := http.NewRequest(http.MethodPost, g.ts.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+g.key)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	elapsed := time.Since(start)
	if err != nil || resp.StatusCode != 200 || !strings.Contains(string(out), "[DONE]") {
		t.Fatalf("after %v: %d %v, stream ended: %.300s", elapsed.Round(time.Millisecond), resp.StatusCode, err, out)
	}
	if elapsed < 4*150*time.Millisecond {
		t.Fatalf("the stream took only %v, so it did not outlast the deadlines", elapsed)
	}
}

// Learning who the caller is after the body has ended sets no deadline: it would
// cancel whatever the request does next.
func TestAuthenticatingAfterTheBodySetsNoDeadline(t *testing.T) {
	limits := httpx.Limits{PreAuthBody: 100 * time.Millisecond, UploadStall: 100 * time.Millisecond}
	var g *limitsGateway // the page asks it who the caller is; assigned before any request
	g = newLimitsGateway(t, limits, func(c *Config) {
		c.Browser = auth.NewBrowser(c.Store, c.Root.Dir, limitsAccess("models:read"), c.Log)
		c.UI = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			g.srv.BrowserPrincipal(r)
			select {
			case <-r.Context().Done():
				w.WriteHeader(500)
			case <-time.After(500 * time.Millisecond):
			}
		})
	})
	req, _ := http.NewRequest(http.MethodPost, g.ts.URL+"/app/upload", strings.NewReader("short"))
	req.Host = "lb.example"
	req.Header.Set("Cookie", uploadSession(t, g))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: the request was cancelled", resp.StatusCode)
	}
}

// Draining a refused body is bounded by its own window, even for a known caller
// whose body keeps arriving a little at a time.
func TestDrainIsNotExtendedByTrickling(t *testing.T) {
	g := newLimitsGateway(t, httpx.Limits{RefusedDrain: 200 * time.Millisecond, UploadStall: 5 * time.Second}, nil)
	conn := g.dial(t)
	fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: 100000\r\n\r\n", g.noScopes)
	go func() {
		for range 100 {
			conn.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err := conn.Write([]byte(" ")); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	start := time.Now()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(line, "403") {
		t.Fatalf("%q %v after %v", line, err, time.Since(start))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("answered after %v, well past the drain window", elapsed)
	}
}

func uploadSession(t *testing.T, g *limitsGateway) string {
	t.Helper()
	token, err := auth.CreateSession(context.Background(), g.store, "owner", auth.Identity{Issuer: "https://idp.example", Subject: "s"})
	if err != nil {
		t.Fatal(err)
	}
	return auth.SessionCookie + "=" + token
}

func limitsAccess(perms ...string) *auth.BrowserAccess {
	return &auth.BrowserAccess{Origin: "https://lb.example", Permissions: perms,
		Provider: auth.Provider{Kind: "direct", Registrations: []auth.Registration{
			{Kind: "oidc", ID: "idp", Name: "IdP", Issuer: "https://idp.example", ClientID: "c", ClientAuthentication: &auth.ClientAuth{Kind: "none"}}}}}
}

// A browser session gets the same budget through /app/api, as a key does.
func TestSlowUploadFromABrowserSession(t *testing.T) {
	limits := httpx.Limits{PreAuthBody: 150 * time.Millisecond, UploadStall: 500 * time.Millisecond}
	g := newLimitsGateway(t, limits, func(c *Config) {
		c.Browser = auth.NewBrowser(c.Store, c.Root.Dir, limitsAccess("inference:chat"), c.Log)
	})
	req, _ := http.NewRequest(http.MethodPost, g.ts.URL+"/app/api/v1/chat/completions", throttled(paddedChat("fake:reply received", 1<<20), 12, 60*time.Millisecond))
	req.Host = "lb.example"
	req.Header.Set("Cookie", uploadSession(t, g))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://lb.example")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("X-Localbase-Ui", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(out), "received") {
		t.Fatalf("%d %.200s", resp.StatusCode, out)
	}
}

// And through a UI page, whose handler asks who the caller is before reading.
func TestSlowUploadToAPage(t *testing.T) {
	limits := httpx.Limits{PreAuthBody: 150 * time.Millisecond, UploadStall: 500 * time.Millisecond}
	var read atomic.Int64
	var g *limitsGateway // the page asks it who the caller is; assigned before any request
	g = newLimitsGateway(t, limits, func(c *Config) {
		c.Browser = auth.NewBrowser(c.Store, c.Root.Dir, limitsAccess("models:read"), c.Log)
		c.UI = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p, _ := g.srv.BrowserPrincipal(r); p == nil {
				w.WriteHeader(401)
				return
			}
			n, err := io.Copy(io.Discard, r.Body)
			read.Store(n)
			if err != nil {
				w.WriteHeader(400)
			}
		})
	})
	body := bytes.Repeat([]byte("x"), 1<<20)
	req, _ := http.NewRequest(http.MethodPost, g.ts.URL+"/app/upload", throttled(body, 12, 60*time.Millisecond))
	req.Host = "lb.example"
	req.Header.Set("Cookie", uploadSession(t, g))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || read.Load() != int64(len(body)) {
		t.Fatalf("status %d, read %d of %d", resp.StatusCode, read.Load(), len(body))
	}
}

// --no-auth has no credential to wait for: the budget applies from the start.
func TestSlowUploadWithoutAuthentication(t *testing.T) {
	limits := httpx.Limits{PreAuthBody: 150 * time.Millisecond, UploadStall: 500 * time.Millisecond}
	g := newLimitsGateway(t, limits, func(c *Config) { c.NoAuth = true })
	req, _ := http.NewRequest(http.MethodPost, g.ts.URL+"/v1/chat/completions", throttled(paddedChat("fake:reply received", 1<<20), 12, 60*time.Millisecond))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(out), "received") {
		t.Fatalf("%d %.200s", resp.StatusCode, out)
	}
}

func TestDefaultLimitsReachTheServer(t *testing.T) {
	var h http.Server
	httpx.Limits{}.Apply(&h)
	if h.ReadHeaderTimeout != httpx.DefaultLimits.ReadHeader || h.IdleTimeout != httpx.DefaultLimits.Idle || h.MaxHeaderBytes != httpx.DefaultLimits.MaxHeaderBytes ||
		h.IdleTimeout <= 0 || h.ReadHeaderTimeout <= 0 {
		t.Fatalf("header %v, idle %v, max header %d", h.ReadHeaderTimeout, h.IdleTimeout, h.MaxHeaderBytes)
	}
	if httpx.DefaultLimits.RefusedDrainBytes > 1<<20 {
		t.Errorf("refused bodies are drained up to %d bytes", httpx.DefaultLimits.RefusedDrainBytes)
	}
}
