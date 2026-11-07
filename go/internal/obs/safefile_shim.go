package obs

import (
	"log/slog"
	"os"

	"github.com/timsexperiments/LocalBase/go/internal/safefile"
)

// The file-safety helpers live in internal/safefile. These names forward to them until each
// importer of obs uses safefile itself; then this file goes.

// UnsafeFileError is safefile.UnsafeFileError.
type UnsafeFileError = safefile.UnsafeFileError

// CheckOwnedRegular is safefile.CheckOwnedRegular.
func CheckOwnedRegular(path string) error { return safefile.CheckOwnedRegular(path) }

// OpenOwnedRegular is safefile.OpenOwnedRegular.
func OpenOwnedRegular(path string, flags int) (*os.File, error) {
	return safefile.OpenOwnedRegular(path, flags)
}

// ReadOwnedRegular is safefile.ReadOwnedRegular.
func ReadOwnedRegular(path string) ([]byte, error) { return safefile.ReadOwnedRegular(path) }

// WritePrivateAtomic is safefile.WritePrivateAtomic.
func WritePrivateAtomic(path string, data []byte) error {
	return safefile.WritePrivateAtomic(path, data)
}

// WritePrivateChecked is safefile.WritePrivateChecked.
func WritePrivateChecked(path string, data []byte) error {
	return safefile.WritePrivateChecked(path, data)
}

// WarnRefusedWrite is safefile.WarnRefusedWrite.
func WarnRefusedWrite(log *slog.Logger, err error) { safefile.WarnRefusedWrite(log, err) }
