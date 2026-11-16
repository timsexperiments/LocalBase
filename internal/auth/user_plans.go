package auth

// The user rules both callers apply (see access_plans.go for the shape). Changing roles, enabling,
// disabling and removing someone already live in this package (ReplaceUserRoles, SetUserStatus,
// RemoveUser), as does InviteUser; what was duplicated was what an invitation says.

// InvitationSignIn is where an invited person signs in: the browser origin's sign-in page.
func InvitationSignIn(cfg *BrowserAccess) string { return cfg.Origin + "/app/login" }

// SendInvitation emails an invitation to sign in at signIn. A caller sends it after the person is
// invited, and reports whether it went (emailDelivered) without the server's words.
func SendInvitation(delivery *EmailConfig, to, signIn string) error {
	subject, body := invitation(signIn)
	return SendEmail(delivery, to, subject, body)
}

// invitation is an invitation's subject and body.
func invitation(signIn string) (subject, body string) {
	return "You were invited to LocalBase", "You were invited to LocalBase. Sign in here:\n\n" + signIn
}
