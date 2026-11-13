package auth

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// NameBindingIssuers gives older email bindings and invitations their sign-in provider
// when only one could be meant (DESIGN §13.6, item 5b), and says what is left. It never
// stops a start: what names no provider grants nothing, and the host commands still work.
func NameBindingIssuers(ctx context.Context, st *store.Store, access *BrowserAccess, log *slog.Logger) {
	adopted, err := AdoptBindingIssuers(ctx, st, access)
	if err != nil {
		log.Error("Naming the sign-in provider on older bindings failed; they grant nothing until it is done.", "event", "auth.issuers-adopt-failed", "error", err.Error())
	} else if adopted > 0 {
		log.Info("Email bindings and invitations now name their sign-in provider.", "event", "auth.issuers-adopted", "count", adopted)
	}
	left, err := CountUnbound(ctx, st.DB)
	if err != nil || len(access.Issuers()) == 0 {
		return // with no provider, nobody can sign in and there is nothing to choose
	}
	for _, group := range []struct {
		kind, what string
		count      int
	}{{"email", "email bindings", left.Emails}, {"email-domain", "domain bindings", left.Domains}, {"invitation", "invitations", left.Invitations}} {
		if group.count > 0 {
			log.Warn(fmt.Sprintf("%d %s are not bound to a sign-in provider, so they grant nothing. Run: local-base access bindings adopt --issuer <issuer>", group.count, group.what),
				"event", "auth.bindings-unbound", "kind", group.kind, "count", group.count)
		}
	}
}
