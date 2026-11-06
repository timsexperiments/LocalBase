// Package childenv decides what a program the gateway starts gets from its environment.
package childenv

import (
	"slices"
	"strings"
)

// A backend runs as the same user, so this isn't a boundary against a hostile one. It keeps the
// gateway's credentials out of a process that doesn't need them, and out of what that process may
// print, crash with or pass on to what it starts.

// secretParts mark a name as holding a credential when one is a whole part of it: HF_TOKEN, and
// not TOKENIZERS_PARALLELISM. A shell that starts `local-base serve` can hold many that aren't
// the gateway's.
var secretParts = []string{"TOKEN", "SECRET", "SECRETS", "PASSWORD", "PASSWD", "CREDENTIAL", "CREDENTIALS", "APIKEY"}

// secretPhrases do the same across parts.
var secretPhrases = []string{"API_KEY", "ACCESS_KEY", "PRIVATE_KEY", "AUTH_SOCK"}

// Clean returns env without what a program the gateway starts shouldn't have. A later entry for a
// name wins in a process's environment, so each entry is judged by itself.
func Clean(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !withheld(name) {
			out = append(out, entry)
		}
	}
	return out
}

func withheld(name string) bool {
	upper := strings.ToUpper(name)
	if strings.HasPrefix(upper, "LOCALBASE_") {
		return true // the gateway's own: its credentials, and how it talks to its service
	}
	// OTLP headers carry a collector's credentials, for all signals or for one.
	if strings.HasPrefix(upper, "OTEL_") && strings.HasSuffix(upper, "HEADERS") {
		return true
	}
	// MYSQL_PWD is a password, and PWD is the working directory.
	if strings.HasSuffix(upper, "_PWD") {
		return true
	}
	for _, part := range strings.Split(upper, "_") {
		if slices.Contains(secretParts, part) {
			return true
		}
	}
	for _, phrase := range secretPhrases {
		if strings.Contains(upper, phrase) {
			return true
		}
	}
	return false
}
