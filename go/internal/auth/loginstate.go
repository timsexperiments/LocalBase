package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"time"
)

// A sign-in in progress keeps nothing on the server, so requests from anyone can't push
// out someone else's. The state carries its own expiry and a MAC, the PKCE verifier and the
// nonce are derived from it under a key that stays here, and the browser holds it in a
// cookie. A state that has signed someone in is remembered, so it works once. A callback
// that fails remembers nothing, so failing them can't push that record out either.

type loginState struct {
	registration, verifier, nonce string
	expires                       time.Time
}

func (b *Browser) loginStateMAC(purpose, registration string, data []byte) []byte {
	m := hmac.New(sha256.New, b.stateKey)
	fmt.Fprintf(m, "%s\x00%s\x00", purpose, registration)
	m.Write(data)
	return m.Sum(nil)
}

// newLoginState makes the state value for a registration.
func (b *Browser) newLoginState(registration string, expires time.Time) string {
	body := make([]byte, loginStateRandom+8)
	if _, err := rand.Read(body[:loginStateRandom]); err != nil {
		panic(err) // never, since Go 1.24
	}
	binary.BigEndian.PutUint64(body[loginStateRandom:], uint64(expires.Unix()))
	return base64.RawURLEncoding.EncodeToString(append(body, b.loginStateMAC("state", registration, body)...))
}

// loginStateOf gives what a state value stands for, if this process made it for a
// registration that is still configured.
func (b *Browser) loginStateOf(value string) (loginState, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != loginStateRandom+8+sha256.Size {
		return loginState{}, false
	}
	body, mac := raw[:loginStateRandom+8], raw[loginStateRandom+8:]
	for id := range b.oidc {
		if hmac.Equal(mac, b.loginStateMAC("state", id, body)) {
			derive := func(purpose string) string {
				return base64.RawURLEncoding.EncodeToString(b.loginStateMAC(purpose, id, body))
			}
			return loginState{registration: id, verifier: derive("verifier"), nonce: derive("nonce"),
				expires: time.Unix(int64(binary.BigEndian.Uint64(body[loginStateRandom:])), 0)}, true
		}
	}
	return loginState{}, false
}

// spendLoginState reports whether this callback may use a state: nobody has finished with
// it and nobody else is using it now. Only completed sign-ins stay here, so forgetting one
// takes thousands of real sign-ins within ten minutes, and even then the provider's
// single-use code holds.
func (b *Browser) spendLoginState(value string, expires time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, used := b.spent[value]; used {
		return false
	}
	t := time.Now()
	for k, until := range b.spent {
		if !until.After(t) {
			delete(b.spent, k)
		}
	}
	for len(b.spent) >= maxSpentStates {
		for k := range b.spent {
			delete(b.spent, k)
			break
		}
	}
	b.spent[value] = expires
	return true
}

// unspendLoginState gives a state back after a callback that signed nobody in.
func (b *Browser) unspendLoginState(value string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.spent, value)
}
