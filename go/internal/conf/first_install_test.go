package conf

import (
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

func TestActivateFirstInstall(t *testing.T) {
	cat, _ := catalog.LoadWithFormer()
	var chat, other, decision, embedding *catalog.Spec
	for _, s := range cat.All() {
		switch {
		case s.Kind != "llm":
		case s.IsDecision():
			decision = s
		case s.IsEmbedding():
			embedding = s
		case chat == nil:
			chat = s
		case other == nil:
			other = s
		}
	}
	if chat == nil || other == nil || decision == nil || embedding == nil {
		t.Fatal("the catalog lacks a model of each task")
	}
	for name, c := range map[string]struct {
		spec      *catalog.Spec
		active    string
		installed []string
		want      bool
	}{
		"no default":             {chat, "", []string{chat.ModelID}, true},
		"default not installed":  {chat, other.ModelID, []string{chat.ModelID}, true},
		"default works":          {chat, other.ModelID, []string{chat.ModelID, other.ModelID}, false},
		"imported default":       {chat, "imported-model", []string{chat.ModelID}, false},
		"already the default":    {chat, chat.ModelID, []string{chat.ModelID}, false},
		"not actually installed": {chat, "", nil, false},
		"a decision model":       {decision, "", []string{decision.ModelID}, false},
		"an embedding model":     {embedding, other.ModelID, []string{embedding.ModelID}, false},
	} {
		cfg := &store.Config{Active: map[string]string{"llm": c.active}, Selected: map[string][]string{"llm": {}}}
		present := map[string]bool{}
		for _, id := range c.installed {
			present[id] = true
		}
		got := ActivateFirstInstall(cfg, c.spec, cat, func(s *catalog.Spec) bool { return present[s.ModelID] })
		if got != c.want {
			t.Errorf("%s: %v", name, got)
		}
		if got && (cfg.Active["llm"] != c.spec.ModelID || len(cfg.Selected["llm"]) != 1) {
			t.Errorf("%s: %v", name, cfg)
		}
		if !got && (cfg.Active["llm"] != c.active || len(cfg.Selected["llm"]) != 0) {
			t.Errorf("%s changed the config: %v", name, cfg)
		}
	}
}
