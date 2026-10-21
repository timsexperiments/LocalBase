package contract

import (
	"bufio"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestClientReplay replays the request shapes of the live clients against
// an already running gateway (-url, with LOCALBASE_CONTRACT_KEY): the M7
// rehearsal's stand-in for naya-device-worker and t3-opencode, which must
// not be pointed at a copy (DESIGN §17.2). naya uses GET /v1/models, GET
// /v1/models/{id}, POST /v1/chat/completions and POST /v1/systemone;
// opencode streams chat completions.
//
//	LOCALBASE_REPLAY=1 LOCALBASE_CONTRACT_KEY=… go test ./contract -run TestClientReplay -args -url http://127.0.0.1:<port>
//
// LOCALBASE_REPLAY_CHAT and LOCALBASE_REPLAY_DECISION name the models
// (default: the gateway's first chat model, and no decision request).
func TestClientReplay(t *testing.T) {
	if os.Getenv("LOCALBASE_REPLAY") == "" || len(targets) != 1 || targets[0].Name != "external" {
		t.Skip("set LOCALBASE_REPLAY=1 and -url (M7 rehearsal)")
	}
	tg := targets[0]
	h := bearer(tg.Key)

	// GET /v1/models: an OpenAI list.
	r := do(t, "GET", tg.BaseURL+"/v1/models", h, "")
	if r.Status != 200 {
		t.Fatalf("models list: %d %s", r.Status, r.Body)
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if json.Unmarshal(r.Body, &list) != nil || list.Object != "list" || len(list.Data) == 0 {
		t.Fatalf("models list shape: %s", r.Body)
	}
	t.Logf("models list: %d models", len(list.Data))

	chat := os.Getenv("LOCALBASE_REPLAY_CHAT")
	if chat == "" {
		chat = list.Data[0].ID
	}
	// GET /v1/models/{id}: every listed model can be retrieved.
	for _, m := range list.Data {
		rr := do(t, "GET", tg.BaseURL+"/v1/models/"+url.PathEscape(m.ID), h, "")
		if rr.Status != 200 || !strings.Contains(string(rr.Body), `"id":"`+m.ID+`"`) {
			t.Errorf("retrieve %s: %d %s", m.ID, rr.Status, rr.Body)
		}
	}
	t.Logf("retrieved every listed model")

	// POST /v1/chat/completions, not streaming.
	hj := bearer(tg.Key)
	hj["Content-Type"] = "application/json"
	body := `{"model":"` + chat + `","max_tokens":8,"messages":[{"role":"user","content":"Reply with one word: ready"}]}`
	r = do(t, "POST", tg.BaseURL+"/v1/chat/completions", hj, body)
	var completion struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role string `json:"role"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if r.Status != 200 || json.Unmarshal(r.Body, &completion) != nil || completion.Object != "chat.completion" ||
		len(completion.Choices) != 1 || completion.Choices[0].Message.Role != "assistant" || completion.Usage == nil {
		t.Fatalf("chat: %d %s", r.Status, r.Body)
	}
	t.Logf("chat (%s): finish %s", chat, completion.Choices[0].FinishReason)

	// POST /v1/chat/completions, streaming: chunks, then [DONE].
	body = `{"model":"` + chat + `","stream":true,"max_tokens":8,"messages":[{"role":"user","content":"Count to three"}]}`
	r = do(t, "POST", tg.BaseURL+"/v1/chat/completions", hj, body)
	if r.Status != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %d %v %s", r.Status, r.Header, r.Body)
	}
	chunks, done := 0, false
	sc := bufio.NewScanner(strings.NewReader(string(r.Body)))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "data: [DONE]":
			done = true
		case strings.HasPrefix(line, "data: "):
			var c struct {
				Object string `json:"object"`
			}
			if json.Unmarshal([]byte(line[6:]), &c) != nil || c.Object != "chat.completion.chunk" {
				t.Fatalf("stream chunk: %s", line)
			}
			chunks++
		}
	}
	if chunks == 0 || !done {
		t.Fatalf("stream: %d chunks, done %v", chunks, done)
	}
	t.Logf("stream: %d chunks, then [DONE]", chunks)

	// POST /v1/systemone: one decision question.
	if decision := os.Getenv("LOCALBASE_REPLAY_DECISION"); decision != "" {
		body = `{"model":"` + decision + `","state":"LocalBase rehearsal.","questions":{"ok":{"type":"noul","instructions":"Is this a test?"}}}`
		r = do(t, "POST", tg.BaseURL+"/v1/systemone", hj, body)
		var d struct {
			Answers map[string]map[string]any `json:"answers"`
		}
		if r.Status != 200 || json.Unmarshal(r.Body, &d) != nil || d.Answers["ok"]["type"] != "noul" {
			t.Fatalf("decision: %d %s", r.Status, r.Body)
		}
		t.Logf("decision (%s): answered", decision)
	}
}
