package gateway

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

// connect asks the running gateway for a model's context: the figure /v1/models gives, to the
// instance token only, and nothing for a model with no plan.
func TestModelContextIsBehindTheToken(t *testing.T) {
	ts := openAPIGateway(t)
	s := runServer(ts)
	release, err := s.cfg.Root.Lock("127.0.0.1", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	get := func(token, model string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+"/_localbase/model-context?model="+url.QueryEscape(model), nil)
		if token != "" {
			req.Header.Set("X-Localbase-Instance-Token", token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var got map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&got)
		return resp.StatusCode, got
	}
	plan, err := s.cfg.LLM.Plan(gatewaytest.ChatModel)
	if err != nil || plan == nil {
		t.Fatalf("plan: %v", err)
	}
	token := s.cfg.Root.Instance().InstanceToken
	if code, got := get(token, gatewaytest.ChatModel); code != 200 || got["contextLength"] != float64(plan.Budget.EffectiveContext) || plan.Budget.EffectiveContext <= 0 {
		t.Errorf("with the token: %d %v (plan %d)", code, got, plan.Budget.EffectiveContext)
	}
	if code, got := get("", gatewaytest.ChatModel); code != 404 || got != nil {
		t.Errorf("without the token: %d %v", code, got)
	}
	if code, _ := get(token, "no-such-model"); code != 404 {
		t.Errorf("a model with no plan: %d", code)
	}
	if code, _ := get(token, ""); code != 404 {
		t.Errorf("no model: %d", code)
	}
}
