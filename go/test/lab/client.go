package lab

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Call is what one HTTP exchange came to.
type Call struct {
	Status     int
	Code       string // the error envelope's code
	RetryAfter string
	RequestID  string
	Body       []byte
	TTFB       time.Duration // until the response headers
	Dur        time.Duration
	Err        error
}

// Client is a gateway client with its own connection pool.
type Client struct {
	Base string
	HTTP *http.Client
}

// NewClient makes a client for g.
func NewClient(base string) *Client {
	return &Client{Base: base, HTTP: &http.Client{Transport: &http.Transport{
		MaxIdleConns: 64, MaxIdleConnsPerHost: 64, IdleConnTimeout: 30 * time.Second, DisableCompression: true}}}
}

// CloseIdle drops pooled connections, so a resource sample does not count them.
func (c *Client) CloseIdle() { c.HTTP.CloseIdleConnections() }

func (c *Client) request(ctx context.Context, method, path, key, ctype string, body []byte) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	return req, nil
}

// Do sends a request and reads the whole response.
func (c *Client) Do(ctx context.Context, method, path, key, ctype string, body []byte) Call {
	began := time.Now()
	req, err := c.request(ctx, method, path, key, ctype, body)
	if err != nil {
		return Call{Err: err}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Call{Err: err, Dur: time.Since(began)}
	}
	defer resp.Body.Close()
	out := headerCall(resp, began)
	out.Body, out.Err = io.ReadAll(resp.Body)
	out.Dur = time.Since(began)
	if out.Status >= 400 {
		out.Code = ErrorCode(out.Body)
	}
	return out
}

// PostJSON posts a JSON body.
func (c *Client) PostJSON(ctx context.Context, path, key string, body any) Call {
	var raw []byte
	switch v := body.(type) {
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	default:
		raw, _ = json.Marshal(v)
	}
	return c.Do(ctx, "POST", path, key, "application/json", raw)
}

func headerCall(resp *http.Response, began time.Time) Call {
	return Call{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After"),
		RequestID: resp.Header.Get("X-Localbase-Request-Id"), TTFB: time.Since(began)}
}

// ErrorCode is the OpenAI error envelope's code; "" when body is not one.
func ErrorCode(body []byte) string {
	var env struct {
		Error *struct {
			Code any `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil || env.Error == nil {
		return ""
	}
	return fmt.Sprint(env.Error.Code)
}

// Stream is one streamed chat completion, as a client sees it.
type Stream struct {
	Call
	FirstToken time.Duration // until the first content chunk
	Content    string
	Done       bool   // saw [DONE]
	Finish     string // the finish_reason
	Chunks     int
	ErrorEvent string // a chunk carrying an error
}

// ChatStream posts a streaming chat request and follows it. onChunk, when set,
// runs after each content chunk and may stop reading by returning false.
func (c *Client) ChatStream(ctx context.Context, key string, body any, onChunk func(*Stream) bool) Stream {
	began := time.Now()
	raw, _ := json.Marshal(body)
	req, err := c.request(ctx, "POST", "/v1/chat/completions", key, "application/json", raw)
	if err != nil {
		return Stream{Call: Call{Err: err}}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Stream{Call: Call{Err: err, Dur: time.Since(began)}}
	}
	defer resp.Body.Close()
	s := Stream{Call: headerCall(resp, began)}
	if resp.StatusCode != 200 {
		s.Body, s.Err = io.ReadAll(resp.Body)
		s.Code, s.Dur = ErrorCode(s.Body), time.Since(began)
		return s
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			s.Done = true
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Error any `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			s.Err = fmt.Errorf("bad chunk: %w", err)
			break
		}
		if chunk.Error != nil {
			s.ErrorEvent = data
			continue
		}
		s.Chunks++
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil {
				s.Finish = *choice.FinishReason
			}
			if choice.Delta.Content != "" {
				if s.FirstToken == 0 {
					s.FirstToken = time.Since(began)
				}
				s.Content += choice.Delta.Content
				if onChunk != nil && !onChunk(&s) {
					s.Dur = time.Since(began)
					return s
				}
			}
		}
	}
	if err := sc.Err(); err != nil && s.Err == nil {
		s.Err = err
	}
	s.Dur = time.Since(began)
	return s
}

// Percentile is the p-th percentile (0-100) of durations, nearest rank.
func Percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p/100*float64(len(sorted))+0.5) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

// Sorted returns a sorted copy.
func Sorted(in []time.Duration) []time.Duration {
	out := append([]time.Duration(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
