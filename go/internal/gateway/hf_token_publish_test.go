package gateway

import (
	"net/http"
	"strings"
	"testing"
)

// Downloads read the token from the running configuration, which saving didn't update.
func TestASavedHFTokenReachesTheRunningConfiguration(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	s := runServer(ts)
	send := func(body string) {
		t.Helper()
		resp, err := http.Post(ts.URL+"/_localbase/hf-token", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", body, resp.StatusCode)
		}
	}
	over := s.cfg.LLM.Config().Clone()
	over.Host = "override.example"
	s.cfg.LLM.SetConfig(over)
	send(`{"action":"save","token":"hf_abcdefgh","validate":false}`)
	if got := s.cfg.LLM.Config(); got.HFToken != "hf_abcdefgh" || got.Host != "override.example" {
		t.Errorf("after saving: host %q, token set %v", got.Host, got.HFToken != "")
	}
	send(`{"action":"clear"}`)
	if got := s.cfg.LLM.Config(); got.HFToken != "" || got.Host != "override.example" {
		t.Errorf("after clearing: host %q, token set %v", got.Host, got.HFToken != "")
	}
}
