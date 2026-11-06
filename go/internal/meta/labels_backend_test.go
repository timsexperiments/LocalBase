package meta

import "testing"

func TestDescribeLaunchShapeNamesABackend(t *testing.T) {
	if got := DescribeLaunchShape("backend=whisper.cpp-metal"); got != "the whisper.cpp-metal backend" {
		t.Errorf("got %q", got)
	}
	if got := DescribeLaunchShape("default"); got != "standard settings" {
		t.Errorf("got %q", got)
	}
}
