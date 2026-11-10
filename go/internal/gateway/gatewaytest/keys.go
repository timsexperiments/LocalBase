package gatewaytest

import "strings"

// KeyAction is an API keys request: action on key id, and the scopes to set, if any.
func KeyAction(action, id string, scopes ...string) string {
	if scopes == nil {
		return `{"action":"` + action + `","keyId":"` + id + `"}`
	}
	return `{"action":"` + action + `","keyId":"` + id + `","scopes":["` + strings.Join(scopes, `","`) + `"]}`
}

// KeysRoute is the API keys API.
const KeysRoute = "/_localbase/api-keys"
