package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every grant, rotation and revocation names who did it, and a refused one is recorded
// too. Nothing a caller sends reaches the log unchecked, and no secret reaches it at all.
func TestKeyChangesAndRefusalsAreAudited(t *testing.T) {
	f := newKeyFixture(t)
	me, narrow := f.key("narrow", 0, "keys:manage", "keys:read")
	wide, _ := f.key("wide", 0, "keys:manage", "access:manage")
	small, _ := f.key("small", 0, "keys:read")
	_, nothing := f.key("nothing", 0, "models:read")

	created := f.expect(narrow, `{"action":"create","name":"made","scopes":["keys:read"]}`, 201, "")
	rotated := f.expect(narrow, KeyAction("rotate", small.ID), 200, "")
	f.expect(narrow, KeyAction("set-scopes", small.ID, "keys:read", "keys:manage"), 200, "")
	f.expect(narrow, KeyAction("revoke", small.ID), 200, "")
	f.expect(narrow, KeyAction("rotate", wide.ID), 403, exceeds)
	f.expect(narrow, `{"action":"create","name":"more","scopes":["access:manage"]}`, 403, exceeds)
	f.post(nothing, "/_localbase/api-keys", `{"action":"create","name":"x","scopes":[]}`)
	f.post("lb_notarealkeyatall", "/_localbase/api-keys", `{"action":"create","name":"x","scopes":[]}`)
	// What a refused caller sends stays out of the log, whatever it is.
	const planted = "lb_plantedSecretValueQ7"
	f.expect(narrow, `{"action":"`+planted+`"}`, 400, "validation_failed")
	f.expect(narrow, `{"action":"rotate","keyId":"`+planted+`"}`, 404, "key_not_found")

	type entry struct {
		Msg, Event, Action, Actor_kind, Actor_id, Target_kind, Target_id, Reason, Scopes string
		Status                                                                           int
	}
	var entries []entry
	var raw []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		raw = append(raw, m)
		get := func(k string) string { s, _ := m[k].(string); return s }
		status, _ := m["status"].(float64)
		entries = append(entries, entry{get("msg"), get("event"), get("action"), get("actor_kind"), get("actor_id"), get("target_kind"), get("target_id"), get("reason"), get("scopes"), int(status)})
	}
	find := func(event, action, target string) *entry {
		for i := range entries {
			if entries[i].Event == event && entries[i].Action == action && (target == "" || entries[i].Target_id == target) {
				return &entries[i]
			}
		}
		t.Errorf("no %s %s %s in:\n%s", event, action, target, f.logs)
		return &entry{}
	}
	const changed, refused = "audit.api-key-changed", "audit.api-key-refused"
	var made struct {
		Key struct {
			ID string `json:"id"`
		} `json:"key"`
	}
	json.Unmarshal([]byte(created), &made)
	if e := find(changed, "create", made.Key.ID); e.Actor_kind != "api-key" || e.Actor_id != me.ID || e.Target_kind != "api-key" || e.Scopes != "keys:read" {
		t.Errorf("create: %+v", e)
	}
	if e := find(changed, "rotate", small.ID); e.Actor_id != me.ID {
		t.Errorf("rotate: %+v", e)
	}
	if e := find(changed, "set-scopes", small.ID); e.Actor_id != me.ID || e.Scopes != "keys:read,keys:manage" && e.Scopes != "keys:manage,keys:read" {
		t.Errorf("set-scopes: %+v", e)
	}
	if e := find(changed, "revoke", small.ID); e.Actor_id != me.ID {
		t.Errorf("revoke: %+v", e)
	}
	if e := find(refused, "rotate", wide.ID); e.Actor_id != me.ID || e.Status != 403 || e.Reason != exceeds || e.Target_kind != "api-key" {
		t.Errorf("refused rotate: %+v", e)
	}
	if e := find(refused, "create", ""); e.Actor_id != me.ID || e.Reason != exceeds {
		t.Errorf("refused create: %+v", e)
	}
	// A caller with no standing is recorded without its request being read.
	var noPermission bool
	for _, e := range entries {
		if e.Event == refused && e.Status == 403 && e.Reason == "insufficient_permissions" && e.Actor_kind == "api-key" && e.Action == "" {
			noPermission = true
		}
		if e.Event == refused && e.Status == 401 {
			t.Errorf("a refusal with no credential was audited: %+v", e)
		}
	}
	if !noPermission {
		t.Errorf("a refusal before the body wasn't audited\n%s", f.logs)
	}
	log := f.logs.String()
	for name, secret := range map[string]string{"created": secretOf(t, created), "rotated": secretOf(t, rotated), "narrow": narrow, "planted": planted, "unknown": "lb_notarealkeyatall"} {
		if strings.Contains(log, secret) {
			t.Errorf("the log holds a secret or something a caller sent: %s", name)
		}
	}
	if len(raw) < 10 {
		t.Errorf("%d log lines:\n%s", len(raw), log)
	}
}

// The same through a real session: the audit names the session's id, never its email.
func TestSessionKeyChangesAreAudited(t *testing.T) {
	f := newKeyFixture(t)
	const owner = "browser:person-hash"
	post, _ := f.sessionPoster(owner, "keys:manage", "keys:read", "models:read")
	wide, _ := f.key("wide", 0, "keys:manage", "access:manage")
	if status, out := post(KeyAction("rotate", wide.ID)); status != 403 {
		t.Fatalf("%d %s", status, out)
	}
	if status, out := post(`{"action":"create","name":"mine","scopes":["keys:read"]}`); status != 201 {
		t.Fatalf("%d %s", status, out)
	}
	if !strings.Contains(f.logs.String(), `"event":"audit.api-key-refused","action":"rotate","actor_kind":"browser-session","actor_id":"`+owner+`"`) {
		t.Errorf("the refusal does not name the session:\n%s", f.logs)
	}
	if !strings.Contains(f.logs.String(), `"event":"audit.api-key-changed","action":"create","actor_kind":"browser-session","actor_id":"`+owner+`"`) {
		t.Errorf("the change does not name the session:\n%s", f.logs)
	}
	if strings.Contains(f.logs.String(), "person@example.com") {
		t.Errorf("the log holds an email:\n%s", f.logs)
	}
}
