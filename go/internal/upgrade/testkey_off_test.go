//go:build !localbase_testkey

package upgrade

import (
	"reflect"
	"testing"
)

// A build without the localbase_testkey tag trusts exactly ReleaseKeys: there's nothing an -X can
// add to (DESIGN §33.5).
func TestADefaultBuildTrustsOnlyTheReleaseKeys(t *testing.T) {
	if got, want := TrustedKeys(), append([]Key{}, ReleaseKeys...); !reflect.DeepEqual(got, want) {
		t.Errorf("TrustedKeys() = %v, want ReleaseKeys %v", got, want)
	}
	if extraKeys() != nil || BuiltWithTestKey() {
		t.Error("a default build has a test key")
	}
}
