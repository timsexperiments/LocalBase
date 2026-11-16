package audit

import (
	"context"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// TemporaryKey creates a key a command uses for a while: named, with scopes, expiring in a day, its
// creation audited with purpose. end deletes it and audits that; its error is the delete's, and on
// one the key stays until it expires.
func (a *Log) TemporaryKey(ctx context.Context, st *store.Store, name string, scopes []string, purpose string) (k *store.APIKey, raw string, end func() error, err error) {
	k, raw, err = st.CreateAPIKeyExpiring(ctx, name, scopes, 1)
	if err != nil {
		return nil, "", nil, err
	}
	a.KeyChange("create", k, "purpose", purpose)
	return k, raw, func() error {
		if err := st.DeleteAPIKey(ctx, k.ID); err != nil {
			return err
		}
		a.KeyChange("delete", k, "purpose", purpose)
		return nil
	}, nil
}
