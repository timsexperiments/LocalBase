// Package instance is the CLI's one client of the gateway that owns a root. It talks only to a
// gateway that has proven it is that root's (service.VerifiedOwnerState), sends the instance token
// straight to it on localhttp's client (no proxy, no redirect), bounds what it reads, and gives a call DefaultTimeout
// only when the caller set no deadline. What a call means, and what to tell the user when it
// fails, stays with the caller.
package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/localhttp"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/service"
)

// DefaultTimeout is the deadline a call gets when its context has none. It is not a ceiling: a
// caller's own deadline stands, longer or shorter.
const DefaultTimeout = 10 * time.Second

// MaxAnswer is the most of an answer a call reads: the gateway's admin answers are small.
const MaxAnswer = 1 << 20

// ErrAnswerTooLarge is returned for an answer larger than MaxAnswer.
var ErrAnswerTooLarge = errors.New("instance: the gateway's answer is larger than the most a call reads")

// Client is a client of one proven gateway.
type Client struct {
	base  string // scheme, host and port, with no trailing slash
	token string
	owner *root.Owner
	http  *http.Client
}

// Dial proves the gateway that owns r and returns a client for it. state and detail are
// VerifiedOwnerState's: c is nil unless state is "active", so nothing is ever sent to a gateway
// that hasn't proven it is r's.
func Dial(r *root.Root) (c *Client, state, detail string) {
	state, o, detail := service.VerifiedOwnerState(r)
	if state != "active" || o == nil {
		return nil, state, detail
	}
	return &Client{base: strings.TrimSuffix(o.HealthURL(), "/health"), token: o.InstanceToken, owner: o, http: localhttp.New(0)}, state, detail
}

// Owner is the proven gateway's owner record.
func (c *Client) Owner() *root.Owner { return c.owner }

// Base is where the gateway answers: scheme, host and port.
func (c *Client) Base() string { return c.base }

// Answer is what the gateway sent back: its status and up to MaxAnswer of its body.
type Answer struct {
	Status int
	Body   []byte
}

// Decode reads the answer's JSON body into v.
func (a *Answer) Decode(v any) error { return json.Unmarshal(a.Body, v) }

// Get calls route with query, which is encoded here, so a value can't change the URL.
func (c *Client) Get(ctx context.Context, route string, query url.Values) (*Answer, error) {
	return c.do(ctx, http.MethodGet, route, query, nil)
}

// Post sends in as the JSON body of a call to route.
func (c *Client) Post(ctx context.Context, route string, in any) (*Answer, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, route, nil, b)
}

func (c *Client) do(ctx context.Context, method, route string, query url.Values, body []byte) (*Answer, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	target := c.base + route
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Localbase-Instance-Token", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxAnswer+1))
	if err != nil {
		return nil, fmt.Errorf("instance: reading the gateway's answer: %w", err)
	}
	if len(b) > MaxAnswer {
		return nil, ErrAnswerTooLarge
	}
	return &Answer{Status: resp.StatusCode, Body: b}, nil
}
