package auth

import (
	"encoding/json"
	"testing"
)

// canonicalizing is a valid policy that Normalize and BindIssuers both change: an unsorted,
// repeated permission list, a mixed-case email and domain, and bindings with no issuer.
func canonicalizing() *Policy {
	admin := "admin"
	return &Policy{
		Roles: []Role{
			{Name: "admin", Description: "Admin", Permissions: []string{"models:read", "access:manage", "models:read"}},
			{Name: "chat", Description: "Chat", Permissions: []string{"inference:chat"}},
		},
		Bindings: []Binding{
			{Kind: "email", Role: "admin", Email: "Owner@Example.com"},
			{Kind: "email-domain", Role: "chat", Domain: "Example.COM"},
		},
		DefaultRole: &admin,
	}
}

func policyBytes(t *testing.T, p *Policy) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A clone can be checked, bound and changed, and the original stays byte-identical.
func TestPolicyCloneLeavesTheOriginalAsItWas(t *testing.T) {
	p := canonicalizing()
	before := policyBytes(t, p)
	c := p.Clone()
	if got := policyBytes(t, c); got != before {
		t.Fatalf("the clone differs:\nwas %s\nnow %s", before, got)
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := c.BindIssuers([]string{"https://id.example.com"}, nil); err != nil {
		t.Fatal(err)
	}
	if policyBytes(t, c) == before {
		t.Fatal("the clone didn't change, so this shows nothing")
	}
	if got := policyBytes(t, p); got != before {
		t.Errorf("checking the clone changed the original:\nwas %s\nnow %s", before, got)
	}
	// Edits in place, which Normalize doesn't make (it replaces each permission list).
	e := p.Clone()
	e.Roles[1].Permissions[0] = "models:read"
	e.Bindings[0].Role = "chat"
	*e.DefaultRole = "chat"
	if got := policyBytes(t, p); got != before {
		t.Errorf("editing the clone changed the original:\nwas %s\nnow %s", before, got)
	}
}

// What Clone is for: a struct copy shares the arrays, so normalizing it normalizes the original.
// (If this ever stops being true, the comments at the two places that normalize a request's
// policy in place need another look.)
func TestAStructCopyOfAPolicySharesItsArrays(t *testing.T) {
	p := canonicalizing()
	before := policyBytes(t, p)
	c := *p
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if policyBytes(t, p) == before {
		t.Error("normalizing a struct copy left the original as it was")
	}
	if p.Bindings[0].Email != "owner@example.com" || len(p.Roles[0].Permissions) != 2 {
		t.Errorf("the original is %s", policyBytes(t, p))
	}
}
