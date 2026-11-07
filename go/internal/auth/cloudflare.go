package auth

import (
	"net/http"
	"strings"
)

// cloudflareIdentity is the identity in a request's Cloudflare Access assertion. A request with no
// assertion, or one that doesn't verify or isn't a person's, has none, and that is no error.
func (b *Browser) cloudflareIdentity(r *http.Request) (*Identity, string, error) {
	token := r.Header.Get("Cf-Access-Jwt-Assertion")
	if token == "" {
		return nil, "", nil
	}
	issuer := "https://" + b.Config.Provider.TeamDomain
	claims, err := b.cfJWKS.Verify(r.Context(), token, VerifyOptions{Algorithms: []string{"RS256"}, Issuer: issuer,
		Audience: b.Config.Provider.Audience, Required: []string{"exp", "sub", "email"}})
	if err != nil {
		return nil, "", nil
	}
	sub, _ := claims["sub"].(string)
	email, _ := claims["email"].(string)
	typ, _ := claims["type"].(string)
	_, cn := claims["common_name"]
	_, sti := claims["service_token_id"]
	if !asciiSubRe.MatchString(sub) || ValidEmail(email) == "" || strings.HasPrefix(strings.ToLower(email), "non_identity@") ||
		typ != "app" || cn || sti || claims["service_token_status"] == true {
		return nil, "", nil
	}
	return &Identity{Issuer: issuer, Subject: sub, VerifiedEmail: email}, ownerID("cloudflare-access", issuer, sub), nil
}

// CloudflareLogout is where a Cloudflare Access session signs out.
func (b *Browser) CloudflareLogout() string {
	if b.Enabled() && b.Config.Provider.Kind == "cloudflare-access" {
		return "/cdn-cgi/access/logout"
	}
	return ""
}
