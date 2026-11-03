package backup

import (
	"errors"
	"strings"
	"testing"
)

// A refusal can quote what's in the file, so it's as careful as the report.
func TestARefusalShowsControlCharactersAsEscapes(t *testing.T) {
	for name, err := range map[string]error{
		"a string": refuse("The file names %s.", "x\x1b[2K\ny"),
		"an error": refuse("It isn't valid: %v", errors.New("x\x1b[2K\ny")),
	} {
		got := err.Error()
		if strings.ContainsRune(got, 0x1b) || strings.Contains(got, "\n") {
			t.Errorf("%s: %q", name, got)
		}
		if !strings.Contains(got, `x\x1b[2K\x0ay`) {
			t.Errorf("%s: it doesn't show it: %q", name, got)
		}
	}
	// The text written here is left as it is.
	if got := refuse("One line.\nAnother: %d.", 3).Error(); got != "One line.\nAnother: 3." {
		t.Errorf("%q", got)
	}
}
