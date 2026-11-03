package gateway

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/connect"
)

// What Connect hands out works (DESIGN §30.7): the curl snippet, run by a real shell
// with a real curl, gets a real answer from a real gateway, using the scopes the page's
// one tap grants. The fields every other client is given reach the same routes.
func TestSnippetsWorkAgainstTheGateway(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl here")
	}
	ts := newTestServer(t)
	s := runServer(ts)
	s.NoAuth = false
	_, key, err := s.Store.CreateAPIKey(t.Context(), "connect-curl", []string{"inference:chat", "models:read"})
	if err != nil {
		t.Fatal(err)
	}
	target := connect.Target{BaseURL: ts.URL + "/v1", Model: chatModel, Key: key}
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
	unfilled := connect.Find("curl", connect.Target{BaseURL: ts.URL + "/v1", Model: chatModel})
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
