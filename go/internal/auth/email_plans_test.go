package auth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckDisableEmail(t *testing.T) {
	magic := Registration{Kind: "magic-link", ID: "email", Name: "Email"}
	for _, tc := range []struct {
		name   string
		access *BrowserAccess
		want   error
	}{
		{"browser access off", nil, nil},
		{"Cloudflare", cloudflare(nil), nil},
		{"no magic link", planDirect(nil, oidcReg("a")), nil},
		{"a magic link", planDirect(nil, oidcReg("a"), magic), ErrEmailInUse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckDisableEmail(tc.access); !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// EmailChanged compares what a save keeps, exactly: nothing in an email configuration is
// normalized on the way to the file.
func TestEmailChangedComparesExactly(t *testing.T) {
	base := func() *EmailConfig {
		return &EmailConfig{Host: "smtp.example.com", Port: 587, Security: "starttls",
			Authentication: EmailAuth{Kind: "password", Username: "user", Password: "one"}, From: "sender@example.com"}
	}
	with := func(change func(*EmailConfig)) *EmailConfig {
		c := base()
		change(c)
		return c
	}
	for _, tc := range []struct {
		name          string
		current, next *EmailConfig
		want          bool
	}{
		{"both off", nil, nil, false},
		{"turned on", nil, base(), true},
		{"turned off", base(), nil, true},
		{"the same", base(), base(), false},
		{"the port", base(), with(func(c *EmailConfig) { c.Port = 465 }), true},
		{"the password", base(), with(func(c *EmailConfig) { c.Authentication.Password = "two" }), true},
		{"no authentication", base(), with(func(c *EmailConfig) { c.Authentication = EmailAuth{Kind: "none"} }), true},
		{"the sender differs in case", base(), with(func(c *EmailConfig) { c.From = "Sender@example.com" }), true},
		{"the host differs in case", base(), with(func(c *EmailConfig) { c.Host = "SMTP.example.com" }), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EmailChanged(tc.current, tc.next); got != tc.want {
				t.Errorf("EmailChanged = %v, want %v", got, tc.want)
			}
			if got := EmailChanged(tc.next, tc.current); got != tc.want {
				t.Errorf("EmailChanged the other way = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRequireEmail(t *testing.T) {
	dir := t.TempDir()
	if _, err := RequireEmail(dir); !errors.Is(err, ErrEmailNotConfigured) {
		t.Errorf("no file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "email-delivery.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var unreadable *EmailLoadError
	if _, err := RequireEmail(dir); !errors.As(err, &unreadable) ||
		unreadable.Message != "Invalid email-delivery.json. Configure email delivery again with the LocalBase CLI." {
		t.Errorf("an unreadable file: %v", err)
	}
	cfg := &EmailConfig{Host: "smtp.example.com", Port: 587, Security: "starttls", Authentication: EmailAuth{Kind: "none"}, From: "sender@example.com"}
	if err := SaveEmail(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if got, err := RequireEmail(dir); err != nil || EmailChanged(got, cfg) {
		t.Errorf("a configured one: %+v, %v", got, err)
	}
}
