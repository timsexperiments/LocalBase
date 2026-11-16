// Package uuidv4 makes random UUIDs: version 4, the RFC 4122 variant, from crypto/rand.
package uuidv4

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns a random version 4 UUID in its canonical form: 8-4-4-4-12 lowercase hex.
func New() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
