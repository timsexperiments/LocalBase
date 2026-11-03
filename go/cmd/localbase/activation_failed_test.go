package main

import (
	"errors"
	"strings"
	"testing"
)

func TestActivationFailedSaysARestartIsOwedIfItCouldNotRecordThat(t *testing.T) {
	got := activationFailed(errors.New("no answer"), nil).Error()
	if !strings.Contains(got, "no answer") || !strings.Contains(got, "Retry local-base restart") || strings.Contains(got, "couldn't be recorded") {
		t.Errorf("%q", got)
	}
	got = activationFailed(errors.New("no answer"), errors.New("database is locked")).Error()
	for _, want := range []string{"no answer", "couldn't be recorded", "database is locked", "Run local-base restart"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
}
