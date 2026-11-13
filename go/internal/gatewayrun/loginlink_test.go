package gatewayrun

import "testing"

func TestServiceServeNeverPrintsLink(t *testing.T) {
	if firstRunLinkAllowed(false, false, "service-token") {
		t.Error("a service-managed gateway must not print a link")
	}
	if firstRunLinkAllowed(true, false, "") || firstRunLinkAllowed(false, true, "") {
		t.Error("--no-auth and a configured provider need no link")
	}
	if !firstRunLinkAllowed(false, false, "") {
		t.Error("a foreground gateway with no access configured should print one")
	}
}
