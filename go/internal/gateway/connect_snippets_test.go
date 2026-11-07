package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/connect"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

// waitForChat waits for the backend to answer before a snippet runs. A snippet's request would
// otherwise be the one that starts it, and a start that fails answers 503 with Retry-After: right
// for a client, which retries, but a snippet runs once. This is about whether a snippet works, not
// whether a backend starts first time, so anything but that 503 still fails at once.
func waitForChat(t *testing.T, base, key string) {
	t.Helper()
	// The deadline covers a request that hangs as well as one that keeps answering 503.
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	for {
		req, err := http.NewRequestWithContext(ctx, "POST", base+"/v1/chat/completions", strings.NewReader(gatewaytest.ChatBody("hi")))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close() // Read to the end already.
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return
		case resp.StatusCode != http.StatusServiceUnavailable:
			t.Fatalf("waiting for the backend: %d %s", resp.StatusCode, body)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the backend still answers 503 after 60 s: %s", body)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// What Connect hands out works (DESIGN §30.7): the curl snippet, run by a real shell
// with a real curl, gets a real answer from a real gateway, using the scopes the page's
// one tap grants. The fields every other client is given reach the same routes.
func TestSnippetsWorkAgainstTheGateway(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl here")
	}
	// Authentication is on from the start: curl is another process, so nothing here could
	// order a change made under a running server with what the server reads.
	ts := openAPIGateway(t)
	s := runServer(ts)
	_, key, err := s.Store.CreateAPIKey(t.Context(), "connect-curl", []string{"inference:chat", "models:read"})
	if err != nil {
		t.Fatal(err)
	}
	waitForChat(t, ts.URL, key)
	target := connect.Target{BaseURL: ts.URL + "/v1", Model: gatewaytest.ChatModel, Key: key}
	c := connect.Find("curl", target)
	var parts []string
	for _, snippet := range c.Snippets {
		parts = append(parts, snippet.Text)
	}
	cmd := exec.Command("sh", "-c", strings.Join(parts, "\n"))
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the snippet failed: %v\n%s", err, out)
	}
	var reply struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	body := out[strings.IndexByte(string(out), '{'):]
	if err := json.Unmarshal(body, &reply); err != nil || reply.Object != "chat.completion" || len(reply.Choices) == 0 {
		t.Fatalf("the snippet's answer: %v\n%s", err, out)
	}

	// An unfilled snippet tells the person, rather than authenticating by accident.
	unfilled := connect.Find("curl", connect.Target{BaseURL: ts.URL + "/v1", Model: gatewaytest.ChatModel})
	var plain []string
	for _, snippet := range unfilled.Snippets {
		plain = append(plain, snippet.Text)
	}
	cmd = exec.Command("sh", "-c", strings.Join(plain, "\n"))
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin"}
	out, err = cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "invalid_api_key") {
		t.Errorf("with the token: %v\n%s", err, out)
	}

	// Every client's base URL is the one that serves the API, with the key it was given.
	for _, client := range connect.Clients(target) {
		text := ""
		for _, snippet := range client.Snippets {
			text += snippet.Text
		}
		if !strings.Contains(text, target.BaseURL) {
			t.Errorf("%s does not carry the base URL", client.ID)
		}
	}
	req, _ := http.NewRequest("GET", target.BaseURL+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("the base URL's /models with the same key: %d", resp.StatusCode)
	}
}
