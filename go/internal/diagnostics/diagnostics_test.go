package diagnostics

import "testing"

func TestAnUnfinishedWriteWinsInTheManifest(t *testing.T) {
	for _, tc := range []struct {
		openFailed, unfinished bool
		want                   string
	}{
		{false, false, ""},
		{true, false, "database_unavailable"},
		{false, true, "unfinished_write"},
		{true, true, "unfinished_write"},
	} {
		if got := databaseProblem(tc.openFailed, tc.unfinished); got != tc.want {
			t.Errorf("open failed %v, unfinished %v: %q, want %q", tc.openFailed, tc.unfinished, got, tc.want)
		}
	}
}
