package conf

import (
	"context"
	"errors"

	"github.com/timsexperiments/LocalBase/go/internal/lease"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// RunningConfig is the configuration a running gateway uses. It can differ from the saved one, by
// the environment and the serve flags.
type RunningConfig interface {
	Config() *store.Config
	SetConfig(*store.Config)
}

// RecordActive makes a model the active one of its kind. The saved configuration is read under the
// root's lease, so that activations don't overwrite each other, or a token or setting saved since
// the gateway started. Only the one value changes: the running configuration's overrides are not
// saved, and the saved one's other values are not replaced by what this process remembers.
func RecordActive(rootDir string, st *store.Store, running RunningConfig, kind, modelID string) error {
	if running.Config().Active[kind] == modelID {
		return nil
	}
	return lease.WithOperation(rootDir, "update runtime configuration", func() error {
		ctx := context.Background()
		saved, err := st.LoadConfig(ctx)
		if errors.Is(err, store.ErrNotConfigured) {
			saved = running.Config().Clone()
		} else if err != nil {
			return err
		}
		saved = saved.Clone()
		if saved.Active == nil {
			saved.Active = map[string]string{}
		}
		saved.Active[kind] = modelID
		if err := st.SaveConfig(ctx, saved); err != nil {
			return err
		}
		next := running.Config().Clone()
		if next.Active == nil {
			next.Active = map[string]string{}
		}
		next.Active[kind], next.HFToken = modelID, saved.HFToken
		running.SetConfig(next)
		return nil
	})
}
