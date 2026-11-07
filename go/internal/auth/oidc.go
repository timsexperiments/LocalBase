package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

type oidcRuntime struct {
	reg  Registration
	mu   sync.Mutex
	meta *oidcMetadata
	keys *JWKS
}

type oidcMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	ResponseTypes         []string `json:"response_types_supported"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
	SigningAlgs           []string `json:"id_token_signing_alg_values_supported"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
}

func s256(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func httpsURL(v string) bool {
	u, err := url.Parse(v)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Fragment == "" && u.Host != ""
}

func (b *Browser) boundedJSON(resp *http.Response, v any) error {
	if resp.ContentLength > maxIdPResponse {
		return errors.New("Identity provider response exceeded its size limit.")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIdPResponse+1))
	if err != nil || len(body) > maxIdPResponse {
		return errors.New("Identity provider response exceeded its size limit.")
	}
	return json.Unmarshal(body, v)
}

func (b *Browser) metadata(ctx context.Context, rt *oidcRuntime) (*oidcMetadata, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.meta != nil {
		return rt.meta, nil
	}
	u, _ := url.Parse(rt.reg.Issuer)
	u.Path = strings.TrimSuffix(u.Path, "/") + "/.well-known/openid-configuration"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("Accept", "application/json")
	resp, err := b.Client.Do(req)
	if err != nil {
		return nil, errors.New("OpenID Connect discovery failed.")
	}
	defer resp.Body.Close()
	var m oidcMetadata
	if resp.StatusCode != 200 || b.boundedJSON(resp, &m) != nil {
		return nil, errors.New("OpenID Connect discovery failed.")
	}
	okAlg := false
	for _, a := range m.SigningAlgs {
		okAlg = okAlg || contains(allowedAlgs, a)
	}
	if m.Issuer != rt.reg.Issuer || !contains(m.ResponseTypes, "code") || !contains(m.CodeChallengeMethods, "S256") ||
		!httpsURL(m.AuthorizationEndpoint) || !httpsURL(m.TokenEndpoint) || !httpsURL(m.JWKSURI) || !okAlg {
		return nil, errors.New("OpenID Connect discovery is incompatible.")
	}
	auth := "none"
	if rt.reg.ClientAuthentication.Kind == "client-secret-basic" {
		auth = "client_secret_basic"
	}
	supported := m.TokenAuthMethods
	if supported == nil {
		supported = []string{"client_secret_basic"}
	}
	if !contains(supported, auth) {
		return nil, errors.New("OpenID Connect token endpoint authentication is incompatible.")
	}
	rt.meta = &m
	rt.keys = &JWKS{URL: m.JWKSURI, Client: b.Client}
	return rt.meta, nil
}

func (b *Browser) exchange(ctx context.Context, rt *oidcRuntime, code string, state loginState) (*Identity, string, error) {
	m, err := b.metadata(ctx, rt)
	if err != nil {
		return nil, "", err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {b.Config.Origin + OIDCCallbackPath}, "code_verifier": {state.verifier}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, m.TokenEndpoint, nil)
	if rt.reg.ClientAuthentication.Kind == "none" {
		form.Set("client_id", rt.reg.ClientID)
	} else {
		esc := func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "%20", "+") }
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(esc(rt.reg.ClientID)+":"+esc(rt.reg.ClientAuthentication.ClientSecret))))
	}
	req.Body = io.NopCloser(strings.NewReader(form.Encode()))
	req.ContentLength = int64(len(form.Encode()))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := b.Client.Do(req)
	if err != nil {
		return nil, "", errors.New("OpenID Connect token exchange failed.")
	}
	defer resp.Body.Close()
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if resp.StatusCode != 200 || b.boundedJSON(resp, &tok) != nil || tok.IDToken == "" || len(tok.IDToken) > 48*1024 {
		return nil, "", errors.New("OpenID Connect token exchange failed.")
	}
	var algs []string
	for _, a := range m.SigningAlgs {
		if contains(allowedAlgs, a) {
			algs = append(algs, a)
		}
	}
	claims, err := rt.keys.Verify(ctx, tok.IDToken, VerifyOptions{Algorithms: algs, Issuer: rt.reg.Issuer,
		Audience: rt.reg.ClientID, Required: []string{"exp", "iat", "sub", "nonce"}})
	if err != nil {
		return nil, "", err
	}
	sub, _ := claims["sub"].(string)
	nonce, _ := claims["nonce"].(string)
	if !asciiSubRe.MatchString(sub) || subtle.ConstantTimeCompare([]byte(nonce), []byte(state.nonce)) != 1 {
		return nil, "", errors.New("OpenID Connect nonce did not match.")
	}
	azp, hasAzp := claims["azp"].(string)
	multiAud := false
	if list, ok := claims["aud"].([]any); ok && len(list) > 1 {
		multiAud = true
	}
	if (multiAud && azp != rt.reg.ClientID) || (!multiAud && hasAzp && azp != rt.reg.ClientID) {
		return nil, "", errors.New("OpenID Connect authorized party did not match.")
	}
	id := &Identity{Issuer: rt.reg.Issuer, Subject: sub}
	if claims["email_verified"] == true {
		if email, _ := claims["email"].(string); ValidEmail(email) != "" {
			id.VerifiedEmail = email
		}
	}
	return id, ownerID("oidc", rt.reg.Issuer, sub), nil
}
