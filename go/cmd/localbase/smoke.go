package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/gateway"
)

// smoke checks that the running gateway serves real inference (#356, #360):
// health, then a 4-token chat or one decision question with a temporary key
// that is deleted afterwards, and optionally a switch to another LLM and back.

type smokeStep struct {
	Name       string  `json:"name"`
	Model      *string `json:"model"`
	OK         bool    `json:"ok"`
	DurationMs int64   `json:"durationMs"`
	Detail     string  `json:"detail"`
}

type smokeFailure struct{ msg string }

func (e *smokeFailure) Error() string { return e.msg }

func failf(format string, a ...any) error { return &smokeFailure{fmt.Sprintf(format, a...)} }

type smoker struct {
	base    string
	key     string
	timeout time.Duration
	client  *http.Client
}

func (s *smoker) do(method, path string, body any, timeout time.Duration) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, s.base+path, rd)
	if s.key != "" {
		req.Header.Set("Authorization", "Bearer "+s.key)
		req.Header.Set("X-Api-Key", s.key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return 0, nil, failf("no response within %gs", timeout.Seconds())
		}
		return 0, nil, failf("request failed: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

func errorReason(status int, body []byte) string {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	var e struct {
		Error *struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != nil {
		msg = e.Error.Message
		if code, ok := e.Error.Code.(string); ok {
			msg += " (" + code + ")"
		}
	}
	if msg == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("HTTP %d: %s", status, msg)
}

func (s *smoker) health() (string, error) {
	status, body, err := s.do("GET", "/health", nil, 5*time.Second)
	if err != nil {
		return "", err
	}
	if status != 200 {
		return "", failf("%s", errorReason(status, body))
	}
	var h struct {
		Status     string `json:"status"`
		Version    string `json:"version"`
		Modalities *struct {
			LLM *struct {
				Configured bool   `json:"configured"`
				State      string `json:"state"`
			} `json:"llm"`
		} `json:"modalities"`
	}
	if json.Unmarshal(body, &h) != nil || h.Status == "" {
		return "", failf("health response has an unexpected shape")
	}
	if h.Status != "ok" {
		return "", failf("gateway reports status %s", h.Status)
	}
	if h.Modalities == nil || h.Modalities.LLM == nil || !h.Modalities.LLM.Configured {
		return "", failf("the gateway has no LLM configured")
	}
	if h.Modalities.LLM.State == "failed" {
		return "", failf("the LLM runtime is in the failed state")
	}
	v := h.Version
	if v == "" {
		v = "unknown"
	}
	mismatch := ""
	if v != gateway.Version {
		mismatch = " (this CLI is " + gateway.Version + ")"
	}
	return fmt.Sprintf("ok, version %s%s, llm %s", v, mismatch, h.Modalities.LLM.State), nil
}

type llmView struct {
	resident string
	decision map[string]bool
}

func (s *smoker) llms() (*llmView, error) {
	status, body, err := s.do("GET", "/_localbase/models", nil, s.timeout)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, failf("could not read model residency: %s", errorReason(status, body))
	}
	var list struct {
		Data []struct {
			ID      string `json:"id"`
			Catalog struct {
				Kind         string `json:"kind"`
				Capabilities *struct {
					Kind string `json:"kind"`
				} `json:"capabilities"`
			} `json:"catalog"`
			Device struct {
				Runtime *struct {
					State string `json:"state"`
				} `json:"runtime"`
			} `json:"device"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &list) != nil {
		return nil, failf("model metadata has an unexpected shape")
	}
	v := &llmView{decision: map[string]bool{}}
	for _, m := range list.Data {
		if m.Catalog.Kind != "llm" {
			continue
		}
		if m.Device.Runtime != nil && v.resident == "" {
			v.resident = m.ID
		}
		if m.Catalog.Capabilities != nil && m.Catalog.Capabilities.Kind == "decision" {
			v.decision[m.ID] = true
		}
	}
	return v, nil
}

func (s *smoker) chat(model string) (string, error) {
	status, body, err := s.do("POST", "/v1/chat/completions", map[string]any{"model": model,
		"messages": []any{map[string]string{"role": "user", "content": "Reply with OK."}}, "max_tokens": 4, "stream": false}, s.timeout)
	if err != nil {
		return "", err
	}
	if status != 200 {
		return "", failf("%s", errorReason(status, body))
	}
	var c struct {
		Choices []struct {
			Message *struct {
				Content          *string `json:"content"`
				Reasoning        *string `json:"reasoning"`
				ReasoningContent *string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			CompletionTokens float64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &c) != nil || len(c.Choices) == 0 {
		return "", failf("chat completion has no choices")
	}
	text := ""
	if m := c.Choices[0].Message; m != nil {
		for _, p := range []*string{m.Content, m.Reasoning, m.ReasoningContent} {
			if p != nil && *p != "" && text == "" {
				text = *p
			}
		}
	}
	tokens := 0
	if c.Usage != nil {
		tokens = int(c.Usage.CompletionTokens)
	}
	if text == "" && tokens <= 0 {
		return "", failf("chat completion returned no output")
	}
	plural := "s"
	if tokens == 1 {
		plural = ""
	}
	return fmt.Sprintf("completed with %d output token%s", tokens, plural), nil
}

func (s *smoker) decide(model string) (string, error) {
	status, body, err := s.do("POST", "/v1/systemone", map[string]any{"model": model, "state": "LocalBase smoke check.",
		"questions": map[string]any{"ok": map[string]string{"type": "noul", "instructions": "Is this a test?"}}}, s.timeout)
	if err != nil {
		return "", err
	}
	if status != 200 {
		return "", failf("%s", errorReason(status, body))
	}
	var d struct {
		Answers map[string]struct {
			Type string   `json:"type"`
			Noul *float64 `json:"noul"`
		} `json:"answers"`
	}
	json.Unmarshal(body, &d)
	a, ok := d.Answers["ok"]
	if !ok || a.Type != "noul" || a.Noul == nil {
		return "", failf("decision response has no answer to the question")
	}
	return fmt.Sprintf("answered a yes/no question (noul %.2f)", *a.Noul), nil
}

func (s *smoker) infer(model string, v *llmView, isDecision func(string) bool) (string, error) {
	if isDecision(model) {
		return s.decide(model)
	}
	return s.chat(model)
}

func (s *smoker) switchModel(from, to string, v *llmView, isDecision func(string) bool) (string, error) {
	_, inferErr := s.infer(to, v, isDecision)
	resident := ""
	if now, err := s.llms(); err == nil {
		resident = now.resident
	}
	if inferErr != nil {
		var sf *smokeFailure
		if !errors.As(inferErr, &sf) {
			return "", inferErr
		}
		if resident == from {
			return "", failf("%s; %s is still resident", sf.msg, from)
		}
		return "", inferErr
	}
	switch {
	case resident == from:
		return "", failf("switch was rolled back; %s is still resident", from)
	case resident != to:
		r := resident
		if r == "" {
			r = "none"
		}
		return "", failf("switch did not land; resident model is %s", r)
	}
	return to + " is resident and answered", nil
}

func smokeCommand() *cli.Command {
	return &cli.Command{Name: "smoke", Summary: "Check that a running gateway serves real inference",
		Examples: []string{"local-base smoke", "local-base smoke --json --switch-to qwen2.5-coder-1.5b-instruct-q4_k_m"},
		Options: []cli.Option{
			{Name: "switch-to", Value: "<model>", Help: "Also switch to this LLM and back; interrupts other clients"},
			{Name: "timeout", Value: "<seconds>", Help: "Per-request timeout, including model loads (default 300)"},
		},
		ExitCodeFor: func(data any) int {
			if m, ok := data.(map[string]any); ok && m["passed"] == false {
				return 1
			}
			return 0
		},
		Run: runSmoke}
}

func runSmoke(c *cli.Ctx) (any, error) {
	timeout := 300
	if n, ok, err := c.Int("timeout"); err != nil || (ok && n <= 0) {
		return nil, cli.Inputf("timeout: must be a positive integer")
	} else if ok {
		timeout = n
	}
	e, err := openEnv(c)
	if err != nil {
		return nil, err
	}
	defer e.Close()
	target := c.String("switch-to")
	if target != "" {
		if s := e.Catalog.ByID(target); s == nil || s.Kind != "llm" {
			return nil, cli.Inputf("Unknown LLM model id: %s", target)
		}
	}
	s := &smoker{timeout: time.Duration(timeout) * time.Second, client: &http.Client{}}
	var steps []smokeStep
	var gatewayURL, failure *string
	step := func(name string, model string, work func() (string, error)) error {
		start := time.Now()
		detail, err := work()
		ok := err == nil
		if err != nil {
			var sf *smokeFailure
			if !errors.As(err, &sf) {
				return err
			}
			detail = sf.msg
		}
		var m *string
		label := name
		if model != "" {
			m, label = &model, name+" "+model
		}
		dur := time.Since(start).Milliseconds()
		steps = append(steps, smokeStep{Name: name, Model: m, OK: ok, DurationMs: dur, Detail: detail})
		if ok {
			c.Info("PASS %s: %s (%d ms)", label, detail, dur)
			return nil
		}
		fmt.Fprintf(c.Stderr, "FAIL %s: %s\n", label, detail)
		f := label + ": " + detail
		failure = &f
		return err
	}
	run := func() error {
		if err := step("health", "", func() (string, error) {
			state, owner, detail := e.Root.OwnerState()
			if state != "active" {
				switch state {
				case "missing":
					detail = "no gateway is running"
				case "stale":
					detail = "the recorded gateway process is no longer running"
				}
				return "", failf("%s for %s", detail, e.Root.Dir)
			}
			s.base = strings.TrimSuffix(owner.HealthURL(), "/health")
			u := s.base
			gatewayURL = &u
			h, err := s.health()
			if err != nil {
				return "", err
			}
			return h + " at " + s.base, nil
		}); err != nil {
			return err
		}
		k, raw, err := e.Store.CreateAPIKeyExpiring(context.Background(), "local-base smoke (temporary)",
			[]string{"inference:chat", "inference:decision", "models:read"}, 1)
		if err != nil {
			return err
		}
		defer e.Store.DeleteAPIKey(context.Background(), k.ID)
		s.key = raw
		v, _ := s.llms()
		isDecision := func(id string) bool {
			if v != nil && v.decision[id] {
				return true
			}
			spec := e.Catalog.ByID(id)
			return spec != nil && spec.IsDecision()
		}
		model := e.Config.Active["llm"]
		if v != nil && v.resident != "" {
			model = v.resident
		}
		kind := "chat"
		if isDecision(model) {
			kind = "decision"
		}
		if err := step(kind, model, func() (string, error) { return s.infer(model, v, isDecision) }); err != nil {
			return err
		}
		if target == "" {
			return nil
		}
		if err := step("switch", target, func() (string, error) {
			if target == model {
				return "", failf("%s is already the resident model", target)
			}
			return s.switchModel(model, target, v, isDecision)
		}); err != nil {
			return err
		}
		return step("switch-back", model, func() (string, error) { return s.switchModel(target, model, v, isDecision) })
	}
	if err := run(); err != nil {
		var sf *smokeFailure
		if !errors.As(err, &sf) {
			return nil, err
		}
	}
	if failure == nil {
		c.Info("Smoke check passed.")
	} else {
		fmt.Fprintf(c.Stderr, "Smoke check failed: %s\n", *failure)
	}
	if steps == nil {
		steps = []smokeStep{}
	}
	return map[string]any{"passed": failure == nil, "gatewayUrl": gatewayURL, "steps": steps, "failure": failure}, nil
}
