//go:build localbase_testkey

package upgrade

import "testing"

// A build tagged localbase_testkey says so, and with no -X it adds no key. That the key does take
// effect with one is the system tests' (a release it signed is accepted).
func TestATestKeyBuildSaysSo(t *testing.T) {
	if !BuiltWithTestKey() {
		t.Error("a tagged build doesn't say it is one")
	}
	if testKey == "" && extraKeys() != nil {
		t.Errorf("no -X, and still a key: %v", extraKeys())
	}
}
