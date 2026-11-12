package auth

import "testing"

// The invitation says what both callers wrote out by hand before it existed: the "was" side is
// that text, copied from cmd/localbase/access_users.go and admin/keys/keys.go.
func TestAnInvitationSaysWhatItSaid(t *testing.T) {
	const signIn = "https://lb.example.com/app/login"
	subject, body := invitation(signIn)
	if subject != "You were invited to LocalBase" || body != "You were invited to LocalBase. Sign in here:\n\n"+signIn {
		t.Errorf("invitation = %q, %q", subject, body)
	}
	if got := InvitationSignIn(planDirect(nil, oidcReg("a"))); got != testOrigin+"/app/login" {
		t.Errorf("InvitationSignIn = %q", got)
	}
}
