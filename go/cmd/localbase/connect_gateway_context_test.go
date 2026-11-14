package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/test/lab"
)

// With a gateway running, connect asks it for the model's context: the figure /v1/models gives,
// which counts the gateway's --ctx-size, where a plan made by the command line alone wouldn't.
func TestConnectAsksTheRunningGatewayForTheContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	w, err := lab.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Build(ctx, false); err != nil {
		t.Fatal(err)
	}
	g, err := w.Start(ctx, lab.Options{CtxSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Stop(30 * time.Second)

	res := lab.NewClient(g.Base).Do(ctx, http.MethodGet, "/v1/models/"+lab.ChatSmall, g.Keys.User, "", nil)
	if res.Err != nil || res.Status != http.StatusOK {
		t.Fatalf("/v1/models: %d %v %.300s", res.Status, res.Err, res.Body)
	}
	var m struct {
		ContextLength int `json:"context_length"`
	}
	if err := json.Unmarshal(res.Body, &m); err != nil || m.ContextLength <= 0 {
		t.Fatalf("/v1/models: %v %s", err, res.Body)
	}

	r := &root.Root{Dir: g.Root}
	st, err := store.OpenReadOnly(filepath.Join(g.Root, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cat, err := catalog.LoadRoot(g.Root, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := st.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{Root: r, Store: st, Catalog: cat, Config: cfg, Host: testHost(t)}
	if got, planned := connectContext(e, lab.ChatSmall); got != m.ContextLength || planned {
		t.Errorf("connect's context %d (planned %v), the gateway's %d", got, planned, m.ContextLength)
	}
	// And that is the gateway's answer, not one the command line would come to alone.
	lib, err := openLibrary(e)
	if err != nil {
		t.Fatal(err)
	}
	llm, _, _ := placementLLM(e, lib)
	defer llm.Close()
	if alone, err := llm.Plan(lab.ChatSmall); err == nil && alone != nil && alone.Budget.EffectiveContext == m.ContextLength {
		t.Errorf("the command line alone also says %d: this doesn't show the gateway was asked", m.ContextLength)
	}
}
