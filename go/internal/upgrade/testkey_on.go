//go:build localbase_testkey

package upgrade

import "strings"

// testKey is set only with -ldflags, in builds tagged localbase_testkey, as "id:base64"
// (DESIGN §33.5). The release build has no such tag: TestReleaseBuildSetsNoTestKey holds its
// recipe to that, and the system tests check its artifact.
var testKey string

// extraKeys is the test key, when one was set.
func extraKeys() []Key {
	if id, pub, ok := strings.Cut(testKey, ":"); ok {
		return []Key{{ID: id, Public: pub}}
	}
	return nil
}

// BuiltWithTestKey says whether this build is tagged to trust a test signing key: this one is.
func BuiltWithTestKey() bool { return true }
