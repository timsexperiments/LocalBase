package obs

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Until each importer uses safefile itself, obs's names must do what safefile's do: refuse a planted
// link, leave its target alone, and say so in the log.
func TestTheForwardedNamesRefuseAPlantedLink(t *testing.T) {
	target, content := victim(t)
	link := filepath.Join(t.TempDir(), "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	wantUnsafe(t, CheckOwnedRegular(link), "symbolic link")
	_, err := OpenOwnedRegular(link, os.O_WRONLY|os.O_TRUNC)
	wantUnsafe(t, err, "symbolic link")
	_, err = ReadOwnedRegular(link)
	wantUnsafe(t, err, "symbolic link")
	refused := WritePrivateChecked(link, []byte("planted"))
	wantUnsafe(t, refused, "symbolic link")
	var logs bytes.Buffer
	WarnRefusedWrite(slog.New(slog.NewTextHandler(&logs, nil)), refused)
	if !strings.Contains(logs.String(), "root.write-refused") {
		t.Errorf("the refusal wasn't logged: %s", logs.String())
	}
	victimUntouched(t, target, content)
	// The atomic write replaces the link itself, and its target stays as it was.
	if err := WritePrivateAtomic(link, []byte("mine")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(link); err != nil || string(got) != "mine" {
		t.Errorf("the write: %q %v", got, err)
	}
	victimUntouched(t, target, content)
}
