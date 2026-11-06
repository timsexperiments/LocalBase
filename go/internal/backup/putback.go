package backup

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// Kinds of secret that --exclude-secrets takes out, as a file records them.
const (
	kindHFToken     = "hf-token"
	kindOTLPHeaders = "otel-headers"
	kindOIDC        = "oidc"
	kindGitHub      = "github"
	kindSMTP        = "smtp"
	kindDownload    = "download"
)

var (
	// The same rules as the commands that take these back: auth's registration ids and acquire's profile ids
	// and header names.
	registrationIDRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
	profileIDRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	headerNameRe     = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,128}$")
	// shellPlain needs no quoting in a POSIX shell.
	shellPlain = regexp.MustCompile(`^[A-Za-z0-9._:/@%+=,-]+$`)
)

// secretName is what a removed secret is called, made here from its kind and id, not taken from a file.
func secretName(r RemovedSecret) string {
	switch r.Kind {
	case kindHFToken:
		return "Hugging Face token"
	case kindOTLPHeaders:
		return "OTLP headers"
	case kindOIDC:
		return "OIDC client secret (" + r.ID + ")"
	case kindGitHub:
		return "GitHub client secret (" + r.ID + ")"
	case kindSMTP:
		return "SMTP password"
	case kindDownload:
		return "download credential (" + r.ID + ")"
	}
	return r.Name
}

// putBackCommand is the command that puts a removed secret back, built from its fields. With check, every
// field from the file has to pass the rule of the command it goes into: a backup is input from someone else,
// so nothing it says is printed as a command. Without, it's this root's own data, for the backup's own output.
func putBackCommand(r RemovedSecret, check bool) (string, bool) {
	ok := func(valid bool) bool { return valid || !check }
	switch r.Kind {
	case kindHFToken:
		return "local-base configure --hf-token <token>", true
	case kindOTLPHeaders:
		return "local-base configure --otel-headers <key=value,...>", true
	case kindSMTP:
		return "local-base access email configure", true
	case kindGitHub:
		if !ok(registrationIDRe.MatchString(r.ID)) {
			return "", false
		}
		return "local-base access github-oauth add --id " + shellArg(r.ID), true
	case kindOIDC:
		if !ok(registrationIDRe.MatchString(r.ID) && plainText(r.Label, 100) && auth.ValidIssuer(r.Issuer) &&
			plainText(r.ClientID, 256) && r.ClientID != "" && !strings.ContainsAny(r.ClientID, " \t")) {
			return "", false
		}
		return fmt.Sprintf("local-base access oidc add --id %s --name %s --issuer %s --client-id %s --client-secret-env <ENVIRONMENT_VARIABLE>",
			shellArg(r.ID), shellArg(r.Label), shellArg(r.Issuer), shellArg(r.ClientID)), true
	case kindDownload:
		if !ok(profileIDRe.MatchString(r.ID)) {
			return "", false
		}
		parts := []string{"local-base auth-profiles add --id " + shellArg(r.ID)}
		for _, o := range r.Origins {
			if !ok(auth.ValidOrigin(o)) {
				return "", false
			}
			parts = append(parts, "--origin "+shellArg(o))
		}
		for _, h := range r.Headers {
			if !ok(headerNameRe.MatchString(h)) {
				return "", false
			}
			parts = append(parts, "--header-env "+shellArg(h)+"=<ENVIRONMENT_VARIABLE>")
		}
		return strings.Join(parts, " "), true
	}
	return "", false
}

// plainText is printable ASCII with no control characters, at most max bytes: what a command can carry
// once quoted, and what reads the same on any terminal.
func plainText(s string, max int) bool {
	if len(s) > max {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

// shellArg quotes a value for a POSIX shell, when it needs it.
func shellArg(s string) string {
	if shellPlain.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// PutBack is a secret a file says --exclude-secrets left out, and the command this version built for it, if
// it could check every part.
type PutBack struct {
	Name    string `json:"name"`
	Command string `json:"command,omitempty"`
}

// putBack is what a restore says about one removed secret. A name this version can make is its own, and
// otherwise the file's, kept short.
func putBack(r RemovedSecret) PutBack {
	command, ok := putBackCommand(r, true)
	if !ok {
		return PutBack{Name: bounded(r.Name, 120)}
	}
	return PutBack{Name: secretName(r), Command: command}
}

// bounded keeps at most max runes of s.
func bounded(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "..."
}
