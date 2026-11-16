package gateway

import (
	"reflect"
	"testing"
)

// A gateway is built from a Config, and nothing is set on it afterwards: the Server has no exported
// field, so a caller can't change one after New. Config holds them all (4d-2b).
func TestServerHasNoExportedFields(t *testing.T) {
	for f := range reflect.TypeFor[Server]().Fields() {
		if f.IsExported() {
			t.Errorf("Server.%s is exported: it belongs in Config", f.Name)
		}
	}
}
