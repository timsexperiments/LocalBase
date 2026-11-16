package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// HasMagicLink reports a magic-link registration.
func (b *Browser) HasMagicLink() *Registration {
	if !b.Direct() {
		return nil
	}
	for i, r := range b.Config.Provider.Registrations {
		if r.Kind == "magic-link" {
			return &b.Config.Provider.Registrations[i]
		}
	}
	return nil
}

func readForm(r *http.Request, max int64, field string) (string, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil || int64(len(body)) > max {
		return "", false
	}
	v, err := url.ParseQuery(string(body))
	if err != nil || len(v) != 1 || len(v[field]) != 1 {
		return "", false
	}
	return v[field][0], true
}

// RequestMagicLink serves POST /app/login/magic. The response is the same
// whether or not the address can sign in.
func (b *Browser) RequestMagicLink(w http.ResponseWriter, r *http.Request, deliver func(to, link string) error) {
	done := func() {
		b.page(w, `<h1 class="welcome-title">Check your email</h1><p class="mt-3 text-muted">If that address can sign in, a link is on its way. It works once, for 15 minutes, and asking again before then doesn’t send another.</p>`, formCSP, "same-origin")
	}
	reg := b.HasMagicLink()
	if reg == nil || r.URL.Query().Get("provider") != reg.ID || !formOK(r, b.Config.Origin) {
		done()
		return
	}
	email, ok := readForm(r, 1024, "email")
	if !ok {
		done()
		return
	}
	if ValidEmail(email) == "" {
		done()
		return
	}
	select {
	case b.magicWork <- struct{}{}:
	default:
		// The page reads the same either way. Whoever asked can ask again.
		now := time.Now().Unix()
		if last := b.magicDropped.Load(); now-last >= 60 && b.magicDropped.CompareAndSwap(last, now) {
			b.Log.Warn("Magic-link requests were dropped because too many are being worked on.", "event", "auth.magic-link-dropped")
		}
		done()
		return
	}
	go func() {
		defer func() { <-b.magicWork }()
		ctx := context.Background()
		token, to, err := IssueMagicLink(ctx, b.Store, email, MagicIssuer(b.Config.Origin))
		if err != nil {
			// Sparingly, since anyone can ask as often as the database keeps failing.
			now := time.Now().Unix()
			if last := b.magicFailed.Load(); now-last >= 60 && b.magicFailed.CompareAndSwap(last, now) {
				b.Log.Error("A magic link could not be issued.", "event", "auth.magic-link-issue-failed", "error", err.Error())
			}
			return
		}
		if token == "" {
			return
		}
		if err := deliver(to, b.Config.Origin+MagicCallbackPath+"#"+token); err != nil {
			if rerr := RevokeMagicLink(ctx, b.Store, token); rerr != nil {
				b.Log.Error("Could not revoke a magic link that was not delivered.", "event", "auth.magic-link-revoke-failed", "error", rerr.Error())
			}
			b.Log.Warn("Magic-link email delivery failed.", "event", "auth.magic-link-delivery-failed", "error", err.Error())
		}
	}()
	done()
}

const redeemScript = `const token=location.hash.slice(1);history.replaceState(null,"",location.pathname);const input=document.querySelector('input[name="token"]');const button=document.querySelector("button");if(/^[A-Za-z0-9_-]{43}$/.test(token)){input.value=token}else{button.disabled=true}`

// MagicLinkPage serves GET /magic-link/callback: the token stays in the URL
// fragment and is posted by a hash-pinned script.
func (b *Browser) MagicLinkPage(w http.ResponseWriter) {
	sum := sha256.Sum256([]byte(redeemScript))
	csp := fmt.Sprintf("default-src 'none'; style-src 'self'; font-src 'self'; script-src 'sha256-%s'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
		base64.StdEncoding.EncodeToString(sum[:]))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	fmt.Fprintf(w, b.pageOpen()+`<h1 class="welcome-title">Sign in to LocalBase</h1><p class="mt-3 text-muted">Continue to use this one-time sign-in link.</p><form method="post" action="%s" class="mt-5"><input name="token" type="hidden"><button class="btn btn-primary w-full" type="submit">Sign in</button></form></main><script>%s</script></html>`,
		MagicCallbackPath, redeemScript)
}

// CompleteMagicLink serves POST /magic-link/callback.
func (b *Browser) CompleteMagicLink(w http.ResponseWriter, r *http.Request) {
	failed := func() { redirect(w, b.Config.Origin+"/app?signin=failed") }
	if b.HasMagicLink() == nil || !formOK(r, b.Config.Origin) {
		failed()
		return
	}
	token, ok := readForm(r, 256, "token")
	if !ok || len(token) > 128 {
		failed()
		return
	}
	id, err := ConsumeMagicLink(r.Context(), b.Store, token, MagicIssuer(b.Config.Origin))
	if err != nil || id == nil {
		failed()
		return
	}
	b.establish(w, r, *id, ownerID("magic-link", id.Issuer, id.Subject))
}
