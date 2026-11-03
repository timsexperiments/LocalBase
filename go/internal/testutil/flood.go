package testutil

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// floodConnsPerWorker is how many connections a flood may open for each worker. A worker
	// needs one, and the rest allow for a connection the server closed for its own reasons.
	floodConnsPerWorker = 8
	// floodDialFailures is how many failed connections stop a flood. A healthy test has none.
	floodDialFailures = 8
)

// Flood sends a great many requests for a test. Any test sending more than a few thousand goes
// through it. send makes one request with the client it's given: workers goroutines call it,
// requests times in all, so it can't call t.Fatal.
//
// A closed connection holds a local port for a while, and everything on the machine shares those
// ports. A test that opens a connection per request can use them up, and then other programs'
// connections fail too, not only its own. So this client keeps its connections, and it never
// opens more than floodConnsPerWorker for each worker. Flood stops and returns an error if it
// would need more, or once floodDialFailures connections have failed to open. The test should
// fail on that error.
//
// send must read each answer to its end, or its connection can't be kept. The gateway closes a
// connection when it refuses a request whose body it hasn't read, so a flood of refusals sends no
// body.
func Flood(workers, requests int, send func(c *http.Client)) error {
	f := &flood{limit: int64(workers * floodConnsPerWorker), connect: (&net.Dialer{Timeout: 10 * time.Second}).DialContext}
	return f.run(workers, requests, send)
}

type flood struct {
	limit         int64
	connect       func(ctx context.Context, network, addr string) (net.Conn, error)
	dials, failed atomic.Int64
}

func (f *flood) run(workers, requests int, send func(c *http.Client)) error {
	c := &http.Client{Transport: &http.Transport{DialContext: f.dial, MaxIdleConnsPerHost: int(f.limit)}}
	defer c.CloseIdleConnections()
	var left atomic.Int64
	left.Store(int64(requests))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for left.Add(-1) >= 0 && f.err() == nil {
				send(c)
			}
		}()
	}
	wg.Wait()
	return f.err()
}

func (f *flood) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if err := f.err(); err != nil {
		return nil, err
	}
	if f.dials.Add(1) > f.limit {
		return nil, f.err()
	}
	conn, err := f.connect(ctx, network, addr)
	if err != nil {
		f.failed.Add(1)
	}
	return conn, err
}

// err says why the flood has stopped, or nil if it hasn't.
func (f *flood) err() error {
	if f.dials.Load() > f.limit {
		return fmt.Errorf("flood stopped: it needed more than %d connections, so they aren't being kept", f.limit)
	}
	if n := f.failed.Load(); n >= floodDialFailures {
		return fmt.Errorf("flood stopped: %d connections failed to open", n)
	}
	return nil
}
