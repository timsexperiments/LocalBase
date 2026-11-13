package install

import (
	"context"
	"fmt"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// ByIDDeps is what installing a catalog model by id needs. Ineligible is the one judgement made
// elsewhere (conf.Ineligible, with the configuration's opt-ins): why a model may not be
// installed, or "" when it may. It is passed in so that install never imports conf.
type ByIDDeps struct {
	Catalog    *catalog.Catalog
	Root       *root.Root
	Store      *store.Store
	Started    *store.Config // the configuration this process started with: its HFToken is the fallback
	Ineligible func(*catalog.Spec) string
}

// InstallFailed is a download or verification that failed, as Model said: a caller can read
// Reason(Err).
type InstallFailed struct{ Err error }

func (e *InstallFailed) Error() string { return e.Err.Error() }
func (e *InstallFailed) Unwrap() error { return e.Err }

// ByID checks eligibility and installs one catalog model, returning its path. An unknown id or an
// ineligible model is refused in its own words; what Model fails with comes back as an
// *InstallFailed.
func ByID(ctx context.Context, d ByIDDeps, id string, report func(Event)) (string, error) {
	s := d.Catalog.ByID(id)
	if s == nil {
		return "", fmt.Errorf("Unknown model id: %s", id)
	}
	if reason := d.Ineligible(s); reason != "" {
		return "", fmt.Errorf("%s: %s", id, reason)
	}
	path, err := Model(ctx, s, d.Root.ModelsDir(s.Kind), Options{HFToken: CurrentHFToken(ctx, d.Store, d.Started),
		AllowPrivate: d.Store.PrivateImportsAllowed(ctx), Report: report})
	if err != nil {
		return "", &InstallFailed{err}
	}
	return path, nil
}

// CurrentHFToken is the saved token now, not when this process started: serve installs on demand
// long after it read its configuration. If the read fails, what it started with is the best left.
func CurrentHFToken(ctx context.Context, st *store.Store, started *store.Config) string {
	if saved, err := st.LoadConfig(ctx); err == nil {
		return saved.HFToken
	}
	return started.HFToken
}
