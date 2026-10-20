package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func pad(b []byte, n int) []byte { return append(make([]byte, n-len(b)), b...) }

func sign(t *testing.T, alg, kid string, key crypto.Signer, claims map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	p, _ := json.Marshal(claims)
	signed := enc(h) + "." + enc(p)
	var sig []byte
	var err error
	switch alg[:2] {
	case "Ed":
		sig = ed25519.Sign(key.(ed25519.PrivateKey), []byte(signed))
	default:
		hh, newHash := hasherFor(alg)
		d := newHash()
		d.Write([]byte(signed))
		sum := d.Sum(nil)
		switch alg[:2] {
		case "RS":
			sig, err = rsa.SignPKCS1v15(rand.Reader, key.(*rsa.PrivateKey), hh, sum)
		case "PS":
			sig, err = rsa.SignPSS(rand.Reader, key.(*rsa.PrivateKey), hh, sum, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		case "ES":
			k := key.(*ecdsa.PrivateKey)
			var r, s *big.Int
			r, s, err = ecdsa.Sign(rand.Reader, k, sum)
			size := (k.Curve.Params().BitSize + 7) / 8
			sig = append(pad(r.Bytes(), size), pad(s.Bytes(), size)...)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + enc(sig)
}

func TestJWTAlgorithms(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p521, _ := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	edPub, edKey, _ := ed25519.GenerateKey(rand.Reader)
	ecJWK := func(kid, crv string, k *ecdsa.PrivateKey) map[string]string {
		size := (k.Curve.Params().BitSize + 7) / 8
		return map[string]string{"kty": "EC", "kid": kid, "crv": crv, "x": enc(pad(k.X.Bytes(), size)), "y": enc(pad(k.Y.Bytes(), size))}
	}
	keys := []map[string]string{
		{"kty": "RSA", "kid": "r", "n": enc(rsaKey.N.Bytes()), "e": enc(big.NewInt(int64(rsaKey.E)).Bytes())},
		ecJWK("e256", "P-256", p256), ecJWK("e384", "P-384", p384), ecJWK("e521", "P-521", p521),
		{"kty": "OKP", "kid": "ed", "crv": "Ed25519", "x": enc(edPub)},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	defer srv.Close()
	set := &JWKS{URL: srv.URL}
	claims := map[string]any{"iss": "https://issuer.example", "aud": "client", "sub": "u1", "exp": time.Now().Add(time.Hour).Unix()}
	opts := VerifyOptions{Algorithms: []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"},
		Issuer: "https://issuer.example", Audience: "client", Required: []string{"exp", "sub"}}
	cases := []struct {
		alg, kid string
		key      crypto.Signer
	}{{"RS256", "r", rsaKey}, {"RS384", "r", rsaKey}, {"RS512", "r", rsaKey}, {"PS256", "r", rsaKey}, {"PS384", "r", rsaKey},
		{"PS512", "r", rsaKey}, {"ES256", "e256", p256}, {"ES384", "e384", p384}, {"ES512", "e521", p521}, {"EdDSA", "ed", edKey}}
	for _, c := range cases {
		tok := sign(t, c.alg, c.kid, c.key, claims)
		if _, err := set.Verify(context.Background(), tok, opts); err != nil {
			t.Errorf("%s: %v", c.alg, err)
		}
		// Tampered payload fails.
		parts := strings.Split(tok, ".")
		bad, _ := json.Marshal(map[string]any{"iss": "https://issuer.example", "aud": "client", "sub": "admin", "exp": claims["exp"]})
		if _, err := set.Verify(context.Background(), parts[0]+"."+enc(bad)+"."+parts[2], opts); err == nil {
			t.Errorf("%s: tampered token verified", c.alg)
		}
	}
	for name, mutate := range map[string]func(map[string]any){
		"expired":  func(m map[string]any) { m["exp"] = time.Now().Add(-time.Minute).Unix() },
		"issuer":   func(m map[string]any) { m["iss"] = "https://evil.example" },
		"audience": func(m map[string]any) { m["aud"] = []string{"other"} },
		"no sub":   func(m map[string]any) { delete(m, "sub") },
	} {
		m := map[string]any{}
		for k, v := range claims {
			m[k] = v
		}
		mutate(m)
		if _, err := set.Verify(context.Background(), sign(t, "RS256", "r", rsaKey, m), opts); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// An algorithm outside the allowed list is refused.
	if _, err := set.Verify(context.Background(), sign(t, "RS256", "r", rsaKey, claims), VerifyOptions{Algorithms: []string{"ES256"}}); err == nil {
		t.Error("disallowed algorithm accepted")
	}
	// alg=none is refused.
	h, _ := json.Marshal(map[string]string{"alg": "none"})
	p, _ := json.Marshal(claims)
	if _, err := set.Verify(context.Background(), enc(h)+"."+enc(p)+".", opts); err == nil {
		t.Error("alg none accepted")
	}
}
