package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
)

func rawChatRequest(t *testing.T, g *limitsGateway, stream bool, content string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"model": chatModel, "stream": stream,
		"messages": []map[string]string{{"role": "user", "content": content}}})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(g.ts.URL)
	return fmt.Sprintf("POST /v1/chat/completions HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		u.Host, g.key, len(body), body)
}

// A client that stops reading lets go of what its request holds, and someone else can chat.
func TestAClientThatStopsReadingLetsGo(t *testing.T) {
	for name, c := range map[string]struct {
		stream  bool
		content string
	}{
		"streaming": {true, "fake:burst 500000"},
		"plain":     {false, "fake:reply " + strings.Repeat("x", 20<<20)},
	} {
		t.Run(name, func(t *testing.T) {
			logs := &syncBuffer{}
			g := newLimitsGateway(t, httpx.Limits{WriteStall: 300 * time.Millisecond}, func(s *Server) { s.Log = slog.New(slog.NewJSONHandler(logs, nil)) })
			u, _ := url.Parse(g.ts.URL)
			conn, err := net.Dial("tcp", u.Host)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(conn, rawChatRequest(t, g, c.stream, c.content)); err != nil {
				t.Fatal(err)
			}
			// The slot is taken, and then given back with the client still connected and not reading.
			deadline := time.Now().Add(60 * time.Second)
			for g.srv.LLM.Snapshot().Active == 0 && g.srv.LLM.Snapshot().Waiting == 0 {
				if time.Now().After(deadline) {
					t.Fatal("the request never started")
				}
				time.Sleep(5 * time.Millisecond)
			}
			for g.srv.LLM.Snapshot().Active != 0 {
				if time.Now().After(deadline) {
					t.Fatalf("a client that isn't reading still holds %d after a minute", g.srv.LLM.Snapshot().Active)
				}
				time.Sleep(20 * time.Millisecond)
			}
			// It's on record as a client that went away, not as an answer that arrived.
			for !strings.Contains(logs.String(), `"status":499`) {
				if time.Now().After(deadline) {
					t.Fatalf("not recorded as a 499: %.600s", logs.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			req, _ := http.NewRequest(http.MethodPost, g.ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"`+chatModel+`","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer "+g.key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("someone else's chat: %d", resp.StatusCode)
			}
		})
	}
}

// Through the gateway and into usage: a client that stopped reading isn't a success and isn't an
// error, and what it asked for still counts, since the work was done. The answer has to be larger than
// the kernel's buffers. A small answer leaves whether or not anyone is reading, so it stays what it was.
func TestAnAnswerCutShortCountsAsCancelled(t *testing.T) {
	for name, c := range map[string]struct {
		stream  bool
		content string
		tokens  bool
	}{
		"plain":     {false, "fake:reply " + strings.Repeat("x", 20<<20), true},
		"streaming": {true, "fake:burst 500000", false},
	} {
		t.Run(name, func(t *testing.T) {
			ts := openAPIGateway(t)
			s := runServer(ts)
			s.Limits.WriteStall = 300 * time.Millisecond
			id, key := oaKey(t, s, "k", auth.DefaultKeyScopes...)
			body, err := json.Marshal(map[string]any{"model": chatModel, "stream": c.stream,
				"messages": []map[string]string{{"role": "user", "content": c.content}}})
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(ts.URL)
			conn, err := net.Dial("tcp", u.Host)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
				u.Host, key, len(body), body)
			var got usage.Stats
			if !waitFor(60*time.Second, func() bool { got = usageOf(t, s, id).Totals; return got.Requests >= 1 }) {
				t.Fatalf("never counted: %+v", got)
			}
			if got.Requests != 1 || got.Cancelled != 1 || got.Failed != 0 || got.Refused != 0 || got.ErrorRate != 0 {
				t.Errorf("requests, cancelled, failed, refused, error rate = %d, %d, %d, %d, %v", got.Requests, got.Cancelled, got.Failed, got.Refused, got.ErrorRate)
			}
			if c.tokens && (got.InputTokens != 3 || got.OutputTokens != 5) {
				t.Errorf("tokens %d in, %d out, where the backend said 3 and 5", got.InputTokens, got.OutputTokens)
			}
		})
	}
}

// A video's job counts once, when it ends, so the 202 that starts it doesn't count, whether or not
// it arrived. That answer is too small to fail on a socket, so this asks directly.
func TestAVideoStartThatFailsToSendCountsNothing(t *testing.T) {
	ts := openAPIGateway(t)
	s := runServer(ts)
	id, _ := oaKey(t, s, "k", auth.DefaultKeyScopes...)
	who := &auth.Principal{Kind: "api-key", ID: id}
	send := func(label string, sent int) {
		t.Helper()
		rec := &httpx.Recorder{Status: sent, SendFailed: true, Auth: who}
		s.recordUsage(rec, label, rec.FinalStatus(), time.Millisecond)
		if err := s.Usage.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	send("/v1/videos", http.StatusAccepted)
	if got := usageOf(t, s, id).Totals; got.Requests != 0 {
		t.Errorf("a video's start counted: %+v", got)
	}
	// Anything else cut short does count, here and on the same route, so that isn't why it's 0.
	send("/v1/videos", http.StatusOK)
	send("/v1/chat/completions", http.StatusOK)
	if got := usageOf(t, s, id).Totals; got.Requests != 2 || got.Cancelled != 2 {
		t.Errorf("answers cut short: %+v", got)
	}
}

// Silence isn't a stall: the deadline is for something being sent, so a model that takes its time
// can take longer than the limit between words and over the whole answer.
func TestAQuietStreamIsNotAStalledOne(t *testing.T) {
	g := newLimitsGateway(t, httpx.Limits{WriteStall: time.Second}, nil)
	req, _ := http.NewRequest(http.MethodPost, g.ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"`+chatModel+`","stream":true,"messages":[{"role":"user","content":"fake:slow 40"}]}`))
	req.Header.Set("Authorization", "Bearer "+g.key)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(out), "[DONE]") {
		t.Fatalf("after %v: %v, stream ended: %.200s", time.Since(start).Round(time.Millisecond), err, out)
	}
	if elapsed := time.Since(start); elapsed < 1500*time.Millisecond {
		t.Fatalf("the stream took only %v, so it didn't outlast the limit", elapsed)
	}
}

// Slow isn't stalled either: a client that keeps reading gets all of an answer that takes longer
// than the limit.
func TestASlowReaderGetsEverything(t *testing.T) {
	g := newLimitsGateway(t, httpx.Limits{WriteStall: 4 * time.Second}, nil)
	u, _ := url.Parse(g.ts.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.(*net.TCPConn).SetReadBuffer(64 << 10); err != nil {
		t.Fatal(err)
	}
	const size = 20 << 20
	if _, err := io.WriteString(conn, rawChatRequest(t, g, false, "fake:reply "+strings.Repeat("x", size))); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, buf := 0, make([]byte, 1<<20)
	for {
		n, err := io.ReadFull(resp.Body, buf)
		got += n
		if err != nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if got < size {
		t.Errorf("%d of at least %d bytes", got, size)
	}
	if elapsed := time.Since(start); elapsed < 4*time.Second {
		t.Errorf("it took only %v, so it didn't outlast the limit", elapsed)
	}
}

// net/http clears the deadline when an answer has gone. This keeps that true of what we rely on: what
// one answer sets doesn't reach the next, or anything net/http writes on its own.
func TestAnEarlierDeadlineDoesNotReachTheNextRequest(t *testing.T) {
	stall := 200 * time.Millisecond
	g := newLimitsGateway(t, httpx.Limits{WriteStall: stall}, nil)
	g.srv.mux.HandleFunc("/test-only-reads", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(204)
	})
	u, _ := url.Parse(g.ts.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rd := bufio.NewReader(conn)
	fmt.Fprintf(conn, "GET /health HTTP/1.1\r\nHost: %s\r\n\r\n", u.Host)
	first, err := http.ReadResponse(rd, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, first.Body)
	first.Body.Close()
	time.Sleep(3 * stall)
	// net/http answers this itself, before the handler has written anything.
	fmt.Fprintf(conn, "POST /test-only-reads HTTP/1.1\r\nHost: %s\r\nContent-Length: 2\r\nExpect: 100-continue\r\n\r\n", u.Host)
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	line, err := rd.ReadString('\n')
	if err != nil || !strings.Contains(line, "100") {
		t.Fatalf("the interim answer: %q %v", line, err)
	}
	if _, err := rd.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	io.WriteString(conn, "ok")
	second, err := http.ReadResponse(rd, nil)
	if err != nil {
		t.Fatalf("the second answer on the connection: %v", err)
	}
	second.Body.Close()
	if second.StatusCode != 204 {
		t.Errorf("%d", second.StatusCode)
	}
}

// Something written long before a handler returns still leaves when it does.
func TestWhatIsBufferedLeavesAtTheEnd(t *testing.T) {
	stall := 200 * time.Millisecond
	g := newLimitsGateway(t, httpx.Limits{WriteStall: stall}, nil)
	g.srv.mux.HandleFunc("/test-only-late", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("written early"))
		time.Sleep(3 * stall)
	})
	resp, err := http.Get(g.ts.URL + "/test-only-late")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || string(b) != "written early" {
		t.Errorf("%q %v", b, err)
	}
}
