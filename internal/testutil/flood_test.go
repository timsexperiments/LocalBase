package testutil

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// get reads the whole answer, as a flood's send must, and says whether there was one.
func get(c *http.Client, url string) bool {
	resp, err := c.Get(url)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return true
}

// Every request is sent once, at the same time as others, over connections that stay open.
func TestAFloodKeepsItsConnections(t *testing.T) {
	const workers, requests = 16, 5000
	var served, opened atomic.Int64
	together := make(chan struct{})
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		if n := served.Add(1); n == workers {
			close(together)
		}
		select {
		case <-together:
		case <-time.After(10 * time.Second):
			t.Error("the workers aren't sending at the same time")
		}
	}))
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	ts.Start()
	defer ts.Close()
	var failed atomic.Int64
	err := Flood(workers, requests, func(c *http.Client) {
		if !get(c, ts.URL) {
			failed.Add(1)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := failed.Load(); n != 0 {
		t.Errorf("%d requests failed", n)
	}
	if n := served.Load(); n != requests {
		t.Errorf("%d requests reached the server, want %d", n, requests)
	}
	if n := opened.Load(); n > workers+4 {
		t.Errorf("%d connections for %d workers", n, workers)
	}
}

// A server that closes each connection is what uses up a machine's ports, so the flood stops at
// its limit, with the rest unsent.
func TestAFloodStopsIfConnectionsAreNotKept(t *testing.T) {
	var served, opened atomic.Int64
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.Header().Set("Connection", "close")
	}))
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	ts.Start()
	defer ts.Close()
	err := Flood(1, 5000, func(c *http.Client) { get(c, ts.URL) })
	if err == nil || !strings.Contains(err.Error(), "aren't being kept") {
		t.Errorf("the error: %v", err)
	}
	if n := served.Load(); n != floodConnsPerWorker {
		t.Errorf("%d requests were served before it stopped, want %d", n, floodConnsPerWorker)
	}
	ts.Close()
	if n := opened.Load(); n != floodConnsPerWorker {
		t.Errorf("%d connections were opened, want %d", n, floodConnsPerWorker)
	}
}

// Connections failing to open can mean the ports have already run out, so it stops trying.
func TestAFloodStopsIfConnectionsFail(t *testing.T) {
	var tried, sent atomic.Int64
	f := &flood{limit: 1 << 20, connect: func(context.Context, string, string) (net.Conn, error) {
		tried.Add(1)
		return nil, errors.New("no")
	}}
	err := f.run(1, 5000, func(c *http.Client) {
		sent.Add(1)
		get(c, "http://127.0.0.1:1/")
	})
	if err == nil || !strings.Contains(err.Error(), "failed to open") {
		t.Errorf("the error: %v", err)
	}
	if n := tried.Load(); n != floodDialFailures {
		t.Errorf("%d connections were tried, want %d", n, floodDialFailures)
	}
	if n := sent.Load(); n != floodDialFailures {
		t.Errorf("%d requests were sent, want %d", n, floodDialFailures)
	}
	// Whatever send does after that, it can't get another connection.
	if _, err := f.dial(context.Background(), "tcp", "127.0.0.1:1"); err == nil {
		t.Error("a stopped flood's dial worked")
	}
	if n := tried.Load(); n != floodDialFailures {
		t.Errorf("a stopped flood tried again: %d", n)
	}
}

// However many ask at once, the limit holds.
func TestAFloodNeverOpensMoreThanItsLimit(t *testing.T) {
	var tried atomic.Int64
	f := &flood{limit: floodConnsPerWorker, connect: func(context.Context, string, string) (net.Conn, error) {
		tried.Add(1)
		client, server := net.Pipe()
		server.Close()
		return client, nil
	}}
	done := make(chan struct{})
	for range 64 {
		go func() {
			defer func() { done <- struct{}{} }()
			if conn, err := f.dial(context.Background(), "tcp", "127.0.0.1:1"); err == nil {
				conn.Close()
			}
		}()
	}
	for range 64 {
		<-done
	}
	if n := tried.Load(); n != floodConnsPerWorker {
		t.Errorf("%d connections were opened, want %d", n, floodConnsPerWorker)
	}
	if f.err() == nil {
		t.Error("it went past its limit and didn't say so")
	}
}

func TestAFloodUnderItsLimitHasNoError(t *testing.T) {
	f := &flood{limit: floodConnsPerWorker, connect: func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		server.Close()
		return client, nil
	}}
	for range floodConnsPerWorker {
		conn, err := f.dial(context.Background(), "tcp", "127.0.0.1:1")
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
	if err := f.err(); err != nil {
		t.Errorf("at its limit: %v", err)
	}
}
