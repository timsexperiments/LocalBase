package conf

import (
	"errors"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

func TestApplyEnv(t *testing.T) {
	t.Setenv("LOCALBASE_HOST", "llm.example")
	t.Setenv("LOCALBASE_STT_HOST", "stt.example")
	t.Setenv("LOCALBASE_PORT", "8081")
	t.Setenv("LOCALBASE_STT_PORT", "8082")
	t.Setenv("LOCALBASE_CTX_SIZE", "4096")
	cfg := &store.Config{Host: "a", SttHost: "b", Port: 1, SttPort: 2, CtxSize: 3}
	if err := ApplyEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "llm.example" || cfg.SttHost != "stt.example" || cfg.Port != 8081 || cfg.SttPort != 8082 || cfg.CtxSize != 4096 {
		t.Errorf("applied: %+v", cfg)
	}
	t.Setenv("LOCALBASE_CTX_SIZE", "100")
	var de *DocumentError
	if err := ApplyEnv(&store.Config{}); !errors.As(err, &de) || de.Message != "LOCALBASE_CTX_SIZE: must be an integer from 2048 to 2147483647" {
		t.Errorf("a context size out of range: %v", err)
	}
}

// Unset variables leave the configuration as it was.
func TestApplyEnvLeavesWhatIsUnset(t *testing.T) {
	for _, name := range []string{"LOCALBASE_HOST", "LOCALBASE_STT_HOST", "LOCALBASE_PORT", "LOCALBASE_STT_PORT", "LOCALBASE_CTX_SIZE"} {
		t.Setenv(name, "")
	}
	cfg := &store.Config{Host: "a", SttHost: "b", Port: 1, SttPort: 2, CtxSize: 3}
	if err := ApplyEnv(cfg); err != nil || cfg.Host != "a" || cfg.SttHost != "b" || cfg.Port != 1 || cfg.SttPort != 2 || cfg.CtxSize != 3 {
		t.Errorf("%+v, %v", cfg, err)
	}
}
