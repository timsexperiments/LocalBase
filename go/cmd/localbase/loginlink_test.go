package main

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

func TestLoginLinkBase(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1:2273": "http://127.0.0.1:2273",
		"0.0.0.0:2273":   "http://127.0.0.1:2273",
		"[::]:2273":      "http://127.0.0.1:2273",
		"localhost:2273": "http://localhost:2273",
		"[::1]:2273":     "http://[::1]:2273",
	} {
		if got, err := loginLinkBase(addr); err != nil || got != want {
			t.Errorf("%s: %q %v", addr, got, err)
		}
	}
	if _, err := loginLinkBase("192.168.1.5:2273"); err == nil {
		t.Error("a gateway with no loopback listener cannot redeem a link")
	}
}
