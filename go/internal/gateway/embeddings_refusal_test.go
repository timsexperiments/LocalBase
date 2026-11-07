package gateway

import (
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

// A chat model cannot produce embeddings. What the backend says about it names its own
// flags and pooling types, which mean nothing to whoever sent the request, so the gateway
// answers first, without starting anything.
func TestEmbeddingsOnAChatModelAreRefusedClearly(t *testing.T) {
	ts, llm, _ := newTestGateway(t, host{})
	r := post(t, ts, "/v1/embeddings", `{"model":"`+gatewaytest.ChatModel+`","input":"hello"}`)
	if r.status != 400 || r.code() != "unsupported_model_capability" {
		t.Fatalf("%d %s", r.status, r.body)
	}
	for _, want := range []string{gatewaytest.ChatModel, "chat model", "embedding model", "/v1/models"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("no %q in %s", want, r.body)
		}
	}
	for _, bad := range []string{"--embeddings", "Pooling"} {
		if strings.Contains(r.body, bad) {
			t.Errorf("%q in %s", bad, r.body)
		}
	}
	if id, _, _ := llm.Running(); id != "" {
		t.Errorf("a refused request started %s", id)
	}
	if r := post(t, ts, "/v1/embeddings", `{"model":"`+embeddingModel+`","input":"hello"}`); r.status != 200 {
		t.Errorf("an embedding model: %d %s", r.status, r.body)
	}
}
