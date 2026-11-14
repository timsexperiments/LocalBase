//go:build !localbase_testkey

package upgrade

// extraKeys is the keys this build trusts beyond ReleaseKeys: none. Only a build tagged
// localbase_testkey has a test key (testkey_on.go). In any other build upgrade.testKey doesn't
// exist, and the linker ignores an -X for a symbol that doesn't exist, so a stray
// -X .../upgrade.testKey= can't make a release trust a key (DESIGN §33.5).
func extraKeys() []Key { return nil }

// BuiltWithTestKey says whether this build is tagged to trust a test signing key. A release never is.
func BuiltWithTestKey() bool { return false }
