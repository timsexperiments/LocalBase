package upgrade

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestClassifyGateway(t *testing.T) {
	const binary = "/bin/local-base"
	ok := GatewayFacts{LockState: "active", OwnerManaged: true, DefinitionInstalled: true, Reachable: true, ServiceProgram: binary, Binary: binary}
	with := func(change func(*GatewayFacts)) GatewayFacts {
		f := ok
		change(&f)
		return f
	}
	if state, err := ClassifyGateway(ok); err != nil || state != GatewayManaged {
		t.Errorf("a managed gateway on this binary: %v %v", state, err)
	}
	for _, lock := range []string{"missing", "stale"} {
		if state, err := ClassifyGateway(GatewayFacts{LockState: lock}); err != nil || state != GatewayNone {
			t.Errorf("%s: %v %v", lock, state, err)
		}
	}
	for name, tc := range map[string]struct {
		f    GatewayFacts
		want string
	}{
		"foreground":            {with(func(f *GatewayFacts) { f.OwnerManaged = false }), "foreground"},
		"manager unreachable":   {with(func(f *GatewayFacts) { f.Reachable = false }), "cannot be reached"},
		"no service definition": {with(func(f *GatewayFacts) { f.DefinitionInstalled = false }), "cannot be reached"},
		"another binary":        {with(func(f *GatewayFacts) { f.ServiceProgram = "/other/local-base" }), "not this binary"},
		"an unreadable lock":    {GatewayFacts{LockState: "invalid", LockDetail: "gateway owner record is invalid"}, "Cannot tell"},
		"a lock being set up":   {GatewayFacts{LockState: "unknown"}, "Cannot tell"},
	} {
		_, err := ClassifyGateway(tc.f)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
		if err != nil && !strings.Contains(err.Error(), "Nothing was changed") {
			t.Errorf("%s: it should say nothing changed: %v", name, err)
		}
	}
}

func TestWaitIdle(t *testing.T) {
	noSleep := func(time.Duration) {}
	if err := WaitIdle(func() (bool, string, error) { return false, "", nil }, 0, noSleep); err != nil {
		t.Errorf("idle: %v", err)
	}
	busy := func() (bool, string, error) { return true, "1 in flight, 0 queued, 0 detached runs", nil }
	err := WaitIdle(busy, 0, noSleep)
	if err == nil || !strings.Contains(err.Error(), "--wait") || !strings.Contains(err.Error(), "--force") || !strings.Contains(err.Error(), "1 in flight") {
		t.Errorf("busy, no wait: %v", err)
	}
	// Finishes in time: goes on.
	left := 3
	calls := 0
	err = WaitIdle(func() (bool, string, error) { calls++; left--; return left > 0, "x", nil }, time.Minute, noSleep)
	if err != nil || calls != 3 {
		t.Errorf("in time: %v after %d asks", err, calls)
	}
	// Does not: refuses, having cut nothing.
	err = WaitIdle(busy, time.Millisecond, func(time.Duration) { time.Sleep(2 * time.Millisecond) })
	if err == nil || !strings.Contains(err.Error(), "nothing was cut") {
		t.Errorf("not in time: %v", err)
	}
	// Cannot ask: it does not guess.
	err = WaitIdle(func() (bool, string, error) { return false, "", errors.New("refused") }, 0, noSleep)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("cannot ask: %v", err)
	}
}

func TestUpgradeOutcome(t *testing.T) {
	for want, got := range map[string]string{
		"applied":      ApplyOutcome(Result{}, nil),
		"rolled-back":  ApplyOutcome(Result{RolledBack: true}, errors.New("PLANTED")),
		"smoke-failed": ApplyOutcome(Result{}, &SmokeFailed{Err: errors.New("PLANTED")}),
		"failed":       ApplyOutcome(Result{}, errors.New("PLANTED")),
	} {
		if got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}

func TestVersionForAudit(t *testing.T) {
	for in, want := range map[string]string{"0.2.0": "0.2.0", "9.9.9": "9.9.9", "not a version": "unknown", "": "unknown",
		"0.2.0\nPLANTED": "unknown", strings.Repeat("9", 100000): "unknown"} {
		if got := VersionForAudit(in); got != want {
			t.Errorf("%.40q: %.40q, want %q", in, got, want)
		}
	}
}

// ParseActivity reads what /_localbase/host answers, whatever that is. This pins today's
// behaviour, odd corners included: a 200 whose body doesn't decode reads as "answered HTTP 200",
// only the first JSON value is read (anything after it is ignored), null and {} read as idle, and
// nothing here bounds the size: a large body is read whole (the client's 5 s timeout is the bound).
func TestParseActivity(t *testing.T) {
	big := `{"pad":"` + strings.Repeat("x", 2<<20) + `","host":{"activity":{"busy":true}}}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		busy   bool
		detail string
		err    string
	}{
		{"busy, with work", 200, `{"host":{"activity":{"busy":true,"runs":{"active":1},"llm":{"inFlight":2,"queued":1},"stt":{"inFlight":1},"video":{"queued":1}}}}`,
			true, "3 in flight, 2 queued, 1 detached runs", ""},
		{"idle", 200, `{"host":{"activity":{"busy":false}}}`, false, "0 in flight, 0 queued, 0 detached runs", ""},
		{"an empty object", 200, `{}`, false, "0 in flight, 0 queued, 0 detached runs", ""},
		{"null", 200, `null`, false, "0 in flight, 0 queued, 0 detached runs", ""},
		{"not a 200", 401, `{"host":{"activity":{"busy":false}}}`, false, "", "the gateway answered HTTP 401"},
		{"a redirect", 307, ``, false, "", "the gateway answered HTTP 307"},
		{"cut off", 200, `{"host":{"activity":`, false, "", "the gateway answered HTTP 200"},
		{"empty", 200, ``, false, "", "the gateway answered HTTP 200"},
		{"not JSON", 200, `<html>busy</html>`, false, "", "the gateway answered HTTP 200"},
		{"a wrong type", 200, `{"host":{"activity":{"busy":"yes"}}}`, false, "", "the gateway answered HTTP 200"},
		{"trailing bytes after the answer", 200, `{"host":{"activity":{"busy":true}}} and more`, true, "0 in flight, 0 queued, 0 detached runs", ""},
		{"a 2 MiB answer", 200, big, true, "0 in flight, 0 queued, 0 detached runs", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			busy, detail, err := ParseActivity(tc.status, strings.NewReader(tc.body))
			if got := fmt.Sprint(err); (tc.err == "" && err != nil) || (tc.err != "" && got != tc.err) {
				t.Errorf("error %v, want %q", err, tc.err)
			}
			if busy != tc.busy || detail != tc.detail {
				t.Errorf("%v %q, want %v %q", busy, detail, tc.busy, tc.detail)
			}
		})
	}
}
