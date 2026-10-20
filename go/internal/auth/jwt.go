package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// JWS verification with JWKS (replaces jose): RS256/384/512, PS256/384/512,
// ES256/384/512 and EdDSA (Ed25519), with issuer, audience, expiry and
// required-claim checks.

// JWKS is a cached remote key set (cooldown 30 s between refetches, cache
// 10 min), like jose's createRemoteJWKSet.
type JWKS struct {
	URL    string
	Client *http.Client

	mu      sync.Mutex
	keys    map[string][]jwk // kid -> keys
	fetched time.Time
}

type jwk struct {
	Kty, Kid, Alg, Use, Crv, N, E, X, Y string
	key                                 crypto.PublicKey
}

func b64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

func (k *jwk) parse() error {
	switch k.Kty {
	case "RSA":
		n, err := b64(k.N)
		if err != nil {
			return err
		}
		e, err := b64(k.E)
		if err != nil || len(e) > 8 {
			return errors.New("bad RSA exponent")
		}
		k.key = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return errors.New("unsupported curve")
		}
		x, err1 := b64(k.X)
		y, err2 := b64(k.Y)
		if err1 != nil || err2 != nil {
			return errors.New("bad EC key")
		}
		pub := &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !curve.IsOnCurve(pub.X, pub.Y) {
			return errors.New("EC point not on curve")
		}
		k.key = pub
	case "OKP":
		x, err := b64(k.X)
		if err != nil || k.Crv != "Ed25519" || len(x) != ed25519.PublicKeySize {
			return errors.New("bad OKP key")
		}
		k.key = ed25519.PublicKey(x)
	default:
		return errors.New("unsupported key type")
	}
	return nil
}

func (s *JWKS) fetch(ctx context.Context) error {
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("JWKS fetch failed: %d", resp.StatusCode)
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return err
	}
	keys := map[string][]jwk{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		if k.parse() == nil {
			keys[k.Kid] = append(keys[k.Kid], k)
		}
	}
	s.keys, s.fetched = keys, time.Now()
	return nil
}

// UnmarshalJSON reads the standard JWK member names.
func (k *jwk) UnmarshalJSON(b []byte) error {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	get := func(name string) string { s, _ := m[name].(string); return s }
	*k = jwk{Kty: get("kty"), Kid: get("kid"), Alg: get("alg"), Use: get("use"), Crv: get("crv"), N: get("n"), E: get("e"),
		X: get("x"), Y: get("y")}
	return nil
}

func (s *JWKS) candidates(ctx context.Context, kid string) ([]jwk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stale := s.keys == nil || time.Since(s.fetched) > 10*time.Minute
	_, known := s.keys[kid]
	if stale || (!known && time.Since(s.fetched) > 30*time.Second) {
		if err := s.fetch(ctx); err != nil && s.keys == nil {
			return nil, err
		}
	}
	if kid != "" {
		return s.keys[kid], nil
	}
	var all []jwk
	for _, ks := range s.keys {
		all = append(all, ks...)
	}
	return all, nil
}

// VerifyOptions are the claim checks.
type VerifyOptions struct {
	Algorithms []string
	Issuer     string
	Audience   string
	Required   []string
	Now        time.Time
}

func hasherFor(alg string) (crypto.Hash, func() hash.Hash) {
	switch alg[2:] {
	case "256":
		return crypto.SHA256, sha256.New
	case "384":
		return crypto.SHA384, sha512.New384
	}
	return crypto.SHA512, sha512.New
}

func verifySig(alg string, key crypto.PublicKey, signed, sig []byte) bool {
	if alg == "EdDSA" {
		k, ok := key.(ed25519.PublicKey)
		return ok && ed25519.Verify(k, signed, sig)
	}
	h, newHash := hasherFor(alg)
	d := newHash()
	d.Write(signed)
	sum := d.Sum(nil)
	switch alg[:2] {
	case "RS":
		k, ok := key.(*rsa.PublicKey)
		return ok && k.N.BitLen() >= 2048 && rsa.VerifyPKCS1v15(k, h, sum, sig) == nil
	case "PS":
		k, ok := key.(*rsa.PublicKey)
		return ok && k.N.BitLen() >= 2048 && rsa.VerifyPSS(k, h, sum, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil
	case "ES":
		k, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return false
		}
		size := (k.Curve.Params().BitSize + 7) / 8
		want := map[string]int{"ES256": 32, "ES384": 48, "ES512": 66}[alg]
		if size != want || len(sig) != 2*size {
			return false
		}
		return ecdsa.Verify(k, sum, new(big.Int).SetBytes(sig[:size]), new(big.Int).SetBytes(sig[size:]))
	}
	return false
}

// Verify checks a compact JWS and returns its claims.
func (s *JWKS) Verify(ctx context.Context, token string, o VerifyOptions) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed token")
	}
	hb, err := b64(parts[0])
	if err != nil {
		return nil, errors.New("malformed header")
	}
	var header struct {
		Alg, Kid, Typ string
		Crit          []string
	}
	if json.Unmarshal(hb, &header) != nil || len(header.Crit) > 0 {
		return nil, errors.New("malformed header")
	}
	if !contains(o.Algorithms, header.Alg) {
		return nil, errors.New("algorithm not allowed")
	}
	sig, err := b64(parts[2])
	if err != nil {
		return nil, errors.New("malformed signature")
	}
	keys, err := s.candidates(ctx, header.Kid)
	if err != nil {
		return nil, err
	}
	signed := []byte(parts[0] + "." + parts[1])
	verified := false
	for _, k := range keys {
		if k.Alg != "" && k.Alg != header.Alg {
			continue
		}
		if verifySig(header.Alg, k.key, signed, sig) {
			verified = true
			break
		}
	}
	if !verified {
		return nil, errors.New("signature verification failed")
	}
	pb, err := b64(parts[1])
	if err != nil {
		return nil, errors.New("malformed payload")
	}
	var claims map[string]any
	dec := json.NewDecoder(strings.NewReader(string(pb)))
	dec.UseNumber()
	if dec.Decode(&claims) != nil || claims == nil {
		return nil, errors.New("malformed payload")
	}
	t := o.Now
	if t.IsZero() {
		t = time.Now()
	}
	num := func(name string) (float64, bool) {
		n, ok := claims[name].(json.Number)
		if !ok {
			return 0, false
		}
		f, err := n.Float64()
		return f, err == nil
	}
	for _, name := range o.Required {
		if _, ok := claims[name]; !ok {
			return nil, fmt.Errorf("missing claim %s", name)
		}
	}
	if exp, ok := num("exp"); ok && float64(t.Unix()) >= exp {
		return nil, errors.New("token expired")
	} else if _, present := claims["exp"]; present && !ok {
		return nil, errors.New("bad exp")
	}
	if nbf, ok := num("nbf"); ok && float64(t.Unix()) < nbf {
		return nil, errors.New("token not yet valid")
	}
	if o.Issuer != "" && claims["iss"] != o.Issuer {
		return nil, errors.New("issuer mismatch")
	}
	if o.Audience != "" {
		match := false
		switch aud := claims["aud"].(type) {
		case string:
			match = aud == o.Audience
		case []any:
			for _, a := range aud {
				match = match || a == o.Audience
			}
		}
		if !match {
			return nil, errors.New("audience mismatch")
		}
	}
	return claims, nil
}
