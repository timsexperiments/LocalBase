package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"html/template"
	"net/http"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// The pages of one-time local sign-in (DESIGN §25). The template gets only these
// fields: nothing else from the server reaches a signed-out visitor.
type localSignIn struct {
	Mode   string // help | redeem | expired | too-many
	Local  bool   // this request passes the loopback rule
	Action string
	Script template.JS
}

func (u *UI) renderLocalSignIn(w http.ResponseWriter, status int, d localSignIn) {
	sum := sha256.Sum256([]byte(auth.LocalRedeemScript))
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; script-src 'sha256-"+
		base64.StdEncoding.EncodeToString(sum[:])+"'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
	var b bytes.Buffer
	if err := u.localSignIn.Execute(&b, d); err != nil {
		http.Error(w, "Sign-in page failed.", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	w.Write(b.Bytes())
}

// LoginHelp says how to sign in when no identity provider is configured.
func (u *UI) LoginHelp(w http.ResponseWriter, r *http.Request, status int) {
	u.renderLocalSignIn(w, status, localSignIn{Mode: "help", Local: auth.LocalRequest(r)})
}

// LoginOnce posts the token in the URL fragment.
func (u *UI) LoginOnce(w http.ResponseWriter, r *http.Request) {
	u.renderLocalSignIn(w, http.StatusOK, localSignIn{Mode: "redeem", Local: true, Action: auth.LocalOncePath, Script: template.JS(auth.LocalRedeemScript)})
}

// LoginExpired answers a wrong, used or expired link, all alike.
func (u *UI) LoginExpired(w http.ResponseWriter, r *http.Request, status int) {
	u.renderLocalSignIn(w, status, localSignIn{Mode: "expired", Local: auth.LocalRequest(r)})
}

// LoginTooMany answers after too many failed tries.
func (u *UI) LoginTooMany(w http.ResponseWriter, r *http.Request) {
	u.renderLocalSignIn(w, http.StatusTooManyRequests, localSignIn{Mode: "too-many", Local: true})
}
