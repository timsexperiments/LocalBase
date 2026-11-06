package gateway

import (
	"encoding/json"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// An input over llama-server's physical batch size is the client's to fix:
// 400 context_length_exceeded with the limit in words, not llama's raw 500
// (operator, Clef Flash qualification; DESIGN §13.6 item 7).
func TestBatchOverflowIs400(t *testing.T) {
	h := &steppedHost{}
	h.avail.Store(50 * memory.GiB)
	ts, _, _ := newTestGateway(t, h)
	q := `"questions":{"ok":{"type":"noul","instructions":"Is this a test?"}}`
	cases := []struct {
		name, path, body, param, message string
	}{
		{"decision", "/v1/systemone", `{"model":"` + decisionModel + `","state":"fake:batch",` + q + `}`, "state",
			"The input is about 27,634 tokens, but this decision model accepts up to 8,192."},
		{"embeddings", "/v1/embeddings", `{"model":"` + embeddingModel + `","input":"fake:batch"}`, "input",
			"The input is about 27,634 tokens, but this embedding model accepts up to 8,192."},
		{"chat", "/v1/chat/completions", `{"model":"` + chatModel + `","messages":[{"role":"user","content":"fake:batch"}]}`, "messages",
			"The input is about 27,634 tokens, but this model accepts up to 8,192."},
		{"chat stream", "/v1/chat/completions", `{"model":"` + chatModel + `","stream":true,"messages":[{"role":"user","content":"fake:batch"}]}`, "messages",
			"The input is about 27,634 tokens, but this model accepts up to 8,192."},
		{"decision, no numbers", "/v1/systemone", `{"model":"` + decisionModel + `","state":"fake:batch-plain",` + q + `}`, "state",
			"The input is larger than this decision model accepts at once. Shorten it and try again."},
	}
	for _, c := range cases {
		r := post(t, ts, c.path, c.body)
		var e struct {
			Error struct {
				Message, Type, Code string
				Param               *string
			}
		}
		json.Unmarshal([]byte(r.body), &e)
		if r.status != 400 || e.Error.Type != "invalid_request_error" || e.Error.Code != "context_length_exceeded" ||
			e.Error.Param == nil || *e.Error.Param != c.param || e.Error.Message != c.message {
			t.Errorf("%s: %d %s", c.name, r.status, r.body)
		}
	}
}

// An empty messages list is the client's mistake: 400 with param messages,
// not llama-server's 500 "No messages provided" (operator observability
// audit, Linux; DESIGN §13.6 item 8).
func TestEmptyMessagesIs400(t *testing.T) {
	h := &steppedHost{}
	h.avail.Store(50 * memory.GiB)
	ts, _, _ := newTestGateway(t, h)
	r := post(t, ts, "/v1/chat/completions", `{"model":"`+chatModel+`","messages":[]}`)
	var e struct {
		Error struct {
			Type, Code string
			Param      *string
		}
	}
	json.Unmarshal([]byte(r.body), &e)
	if r.status != 400 || e.Error.Type != "invalid_request_error" || e.Error.Code != "validation_failed" || e.Error.Param == nil || *e.Error.Param != "messages" {
		t.Errorf("%d %s", r.status, r.body)
	}
}
