package keys

import (
	"context"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

// Two managers revoking each other at once must not leave nobody. Every request reaches
// the point just before its write together with the other, if nothing keeps them apart.
func TestManagersRevokingEachOtherLeaveOneStanding(t *testing.T) {
	k := newKeysHarness(t)
	a, aSecret := k.key("a", "keys:manage", "access:manage")
	b, bSecret := k.key("b", "keys:manage", "access:manage")
	const both = 2
	k.holdBeforeWrite()
	var wg sync.WaitGroup
	statuses := make([]int, both)
	for i, call := range [][2]string{{aSecret, b.ID}, {bSecret, a.ID}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], _ = k.post(call[0], gatewaytest.KeysRoute, gatewaytest.KeyAction("revoke", call[1]))
		}()
	}
	wg.Wait()
	alive := 0
	for _, id := range []string{a.ID, b.ID} {
		stored, err := k.st.APIKeyByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if auth.ActiveKey(*stored) {
			alive++
		}
	}
	if alive != 1 {
		t.Fatalf("%d managers left after they revoked each other (statuses %v)", alive, statuses)
	}
}

// A key change and an access change share one lock: each check reads what the other
// writes, so neither may run between the other's check and its write.
func TestKeyAndAccessChangesCannotRaceToNobody(t *testing.T) {
	k := newKeysHarness(t)
	ctx := context.Background()
	call := func(path, body string) int {
		status, _ := k.post("", path, body) // as the host
		return status
	}
	if got := call(gatewaytest.AccessRoute, gatewaytest.ConfigureCloudflare); got != 200 {
		t.Fatal(got)
	}
	const bound = `{"kind":"email","role":"admin","email":"person@example.test"}`
	if got := call(gatewaytest.AccessRoute, gatewaytest.PolicyRequest("null", gatewaytest.ChatRole+","+gatewaytest.AdminRole, bound, "null")); got != 200 {
		t.Fatal(got)
	}
	if got := call(gatewaytest.AccessRoute, `{"action":"invite-user","email":"person@example.test","roles":["chat"]}`); got != 201 {
		t.Fatal(got)
	}
	// One key and one person can manage access. Each is being removed by the other side.
	key, _ := k.key("manager", "access:manage")
	k.holdBeforeWrite()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); call(gatewaytest.KeysRoute, gatewaytest.KeyAction("revoke", key.ID)) }()
	go func() {
		defer wg.Done()
		call(gatewaytest.AccessRoute, `{"action":"disable-user","email":"person@example.test"}`)
	}()
	wg.Wait()
	cfg, err := auth.LoadAccess(k.root.Dir)
	if err != nil {
		t.Fatal(err)
	}
	g, err := auth.LoadGuard(ctx, k.st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !g.HasManager() {
		t.Fatal("the two changes together left nobody able to manage access")
	}
}
