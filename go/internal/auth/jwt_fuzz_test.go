package auth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

type noNetwork struct{}

func (noNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no network here")
}

// A token is the first thing a stranger can send a gateway that accepts them. Nothing fuzzed can be
// signed by this key, so nothing may be accepted. Plain `go test` runs the seeds, and scripts/fuzz.sh
// fuzzes on demand.
func FuzzJWKSVerify(f *testing.F) {
	enc := base64.RawURLEncoding
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		f.Fatal(err)
	}
	key := jwk{Kty: "OKP", Kid: "k", Crv: "Ed25519", X: enc.EncodeToString(pub)}
	if err := key.parse(); err != nil {
		f.Fatal(err)
	}
	set := &JWKS{URL: "https://keys.invalid/", Client: &http.Client{Transport: noNetwork{}},
		keys:    map[string][]jwk{"k": {key}},
		fetched: time.Now()}
	opts := VerifyOptions{Algorithms: []string{"RS256", "PS256", "ES256", "ES384", "ES512", "EdDSA"}, Issuer: "i", Audience: "a", Required: []string{"exp", "sub"}}
	part := func(s string) string { return enc.EncodeToString([]byte(s)) }
	for _, seed := range []string{
		"", ".", "..", "a.b.c",
		part(`{"alg":"EdDSA","kid":"k"}`) + "." + part(`{"iss":"i","aud":"a","sub":"s","exp":9999999999}`) + "." + part("x"),
		part(`{"alg":"none"}`) + "." + part(`{}`) + ".",
		part(`{"alg":"HS256","kid":"k"}`) + "." + part(`{}`) + "." + part("x"),
		part(`{"alg":"ES256","kid":"k"}`) + "." + part(`[]`) + "." + part(strings.Repeat("x", 64)),
		part(`{"alg":1,"kid":[]}`) + "." + part(`null`) + "." + part("x"),
		part(`{"alg":"EdDSA","kid":"unknown"}`) + "." + part(`{"exp":"soon","aud":[1,null]}`) + "." + part("x"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, token string) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if claims, err := set.Verify(ctx, token, opts); err == nil {
			t.Fatalf("accepted %q: %v", token, claims)
		}
	})
}
