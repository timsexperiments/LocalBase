package auth

import (
	"fmt"
	"html"
	"net/http"
)

// Stylesheet is the app's style sheet, with its version. The web UI sets it, so these pages
// look like the rest of the app.
var Stylesheet = "/app/assets/app.css"

// pageOpen is the head of a sign-in page: the same page as the app's other sign-in pages.
func pageOpen() string {
	return `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Sign in to LocalBase</title><link rel="stylesheet" href="` + html.EscapeString(Stylesheet) + `"><main class="welcome signin"><span aria-hidden="true" class="welcome-mark"><svg viewBox="0 0 24 24" class="mark"><path fill="currentColor" d="M3 22V11a9 9 0 0 1 18 0v11Z"/><path class="mark-light" d="M6.6 22V11a5.4 5.4 0 0 1 10.8 0v11Z"/></svg></span>`
}

func page(w http.ResponseWriter, body, csp, referrer string) {
	pageAt(w, http.StatusOK, body, csp, referrer)
}

func pageAt(w http.ResponseWriter, status int, body, csp, referrer string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", referrer)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	fmt.Fprint(w, pageOpen()+body+"</main></html>")
}

const formCSP = "default-src 'none'; style-src 'self'; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// noAccessPage tells someone with no permissions so, and whoever runs this LocalBase the
// one thing to do about it, with the command for how far it has been set up.
func noAccessPage(email, issuer string, hasPolicy bool) string {
	account, address := "that account", "&lt;your address&gt;"
	if email != "" {
		account = html.EscapeString(email)
		address = account
	}
	command := "local-base access policy starter --email " + address
	then := "If this LocalBase is yours, run this on its computer, then sign in again:"
	if hasPolicy {
		command = "local-base access users invite --email " + address + " --roles &lt;role&gt;"
		then = "Whoever runs this LocalBase can give it access, by running this on its computer:"
	}
	if issuer != "" {
		command += " --issuer " + html.EscapeString(issuer)
	}
	return "<h1>This account has no access here</h1><p>You signed in as " + account + ", but this LocalBase gives it nothing, so you were not signed in.</p><p>" + then + "</p><pre>" + command + "</pre>"
}
