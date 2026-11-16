package uuidv4

import (
	"regexp"
	"testing"
)

// A UUID is lowercase 8-4-4-4-12 hex, version 4 (the 13th digit), with the RFC 4122 variant (the 17th
// digit is 8, 9, a or b), and two are never the same.
func TestNewIsARandomVersion4UUID(t *testing.T) {
	form := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for range 1000 {
		id := New()
		if !form.MatchString(id) {
			t.Fatalf("%q isn't a version 4 UUID", id)
		}
		if seen[id] {
			t.Fatalf("%q came twice", id)
		}
		seen[id] = true
	}
}
