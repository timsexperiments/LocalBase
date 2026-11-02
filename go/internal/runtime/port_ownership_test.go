package runtime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// occupy runs another server on port, answering every health probe with 200: what
// a live gateway's backend looks like to a scratch gateway given the same port.
func occupy(t *testing.T, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"status":"ok"}`)
	})}}
	srv.Start()
	t.Cleanup(srv.Close)
}

func TestLLMRefusesOccupiedPort(t *testing.T) {
	m := newTestLLM(t, &bigHost{avail: 50_000_000_000})
	var logs syncBuffer
	m.d.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	port := m.port()
	occupy(t, port)
	l, err := m.Acquire(context.Background(), modelA)
	if err == nil {
		l.Release()
		t.Fatalf("started against a port another process holds: %s", l.BaseURL)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(port)) {
		t.Errorf("the error names no port: %v", err)
	}
	if strings.Contains(logs.String(), "runtime.ready") {
		t.Error("runtime.ready from a process the gateway did not start")
	}
	if id, _, _ := m.Running(); id != "" {
		t.Errorf("running %q", id)
	}
}

func TestServiceRefusesOccupiedPort(t *testing.T) {
	s, _ := newTestService(t)
	occupy(t, s.d.Port)
	l, err := s.Acquire(context.Background(), "a")
	if err == nil {
		l.Release()
		t.Fatalf("started against a port another process holds: %s", l.BaseURL)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(s.d.Port)) {
		t.Errorf("the error names no port: %v", err)
	}
}

// A port taken after the check: the child fails to bind and exits, and the other
// server answers its probe. A healthy answer does not count if the child is gone.
func TestWaitHealthyRejectsExitedChild(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	p, err := StartProcess("child", "/bin/sh", []string{"-c", "exit 1"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.WaitHealthy(context.Background(), srv.URL, 5*time.Second); err == nil {
		t.Error("ready, from a server that is not the exited child")
	}
}

func TestCheckPortFreeWaitsForARelease(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	time.AfterFunc(200*time.Millisecond, func() { ln.Close() })
	if err := checkPortFree("127.0.0.1", port); err != nil {
		t.Errorf("a backend just stopped: %v", err)
	}
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	port = busy.Addr().(*net.TCPAddr).Port
	err = checkPortFree("127.0.0.1", port)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(port)) {
		t.Errorf("a held port: %v", err)
	}
}

// On macOS a bind can succeed beside a wildcard listener, so the check connects too.
func TestCheckPortFreeSeesWildcardListener(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := checkPortFree("127.0.0.1", ln.Addr().(*net.TCPAddr).Port); err == nil {
		t.Error("a wildcard listener on the port went unseen")
	}
}
