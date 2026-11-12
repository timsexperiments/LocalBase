package auth

import (
	"bytes"
	"encoding/json"
	"errors"
)

// The email delivery rules both callers apply (see access_plans.go for the shape). Configuring
// email delivery has no plan: it never reads the current file, so it can replace one that can't be
// read, and EmailConfig.Validate is its whole check.

// ErrEmailInUse: a magic-link registration needs email delivery, so it can't be disabled.
var ErrEmailInUse = errors.New("email delivery is in use by a magic-link registration")

// CheckDisableEmail checks that email delivery may be disabled under access, the browser access
// configuration (nil when off): not while a magic-link registration needs it.
func CheckDisableEmail(access *BrowserAccess) error {
	if access != nil && access.Provider.Kind == "direct" {
		for _, r := range access.Provider.Registrations {
			if r.Kind == "magic-link" {
				return ErrEmailInUse
			}
		}
	}
	return nil
}

// EmailChanged reports whether next differs from current in what a save would keep; either may be
// nil, for email delivery off. A save keeps every field as given, so they are compared exactly:
// case, and the password, included.
func EmailChanged(current, next *EmailConfig) bool {
	if current == nil || next == nil {
		return current != next
	}
	a, _ := json.Marshal(current)
	b, _ := json.Marshal(next)
	return !bytes.Equal(a, b)
}

// EmailLoadError: email-delivery.json is there and can't be read. Message is LoadEmail's own
// words, which say what to do. It is the machine's state, not a request's fault.
type EmailLoadError struct{ Message string }

func (e *EmailLoadError) Error() string { return e.Message }

// RequireEmail is email delivery for something that needs it: ErrEmailNotConfigured when there is
// none, and an *EmailLoadError when the file can't be read, so a broken file never reads as "not
// configured".
func RequireEmail(rootDir string) (*EmailConfig, error) {
	c, err := LoadEmail(rootDir)
	if err != nil {
		return nil, &EmailLoadError{err.Error()}
	}
	if c == nil {
		return nil, ErrEmailNotConfigured
	}
	return c, nil
}
