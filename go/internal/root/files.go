package root

import "os"

// syncFile is (*os.File).Sync, which on macOS is F_FULLFSYNC. A test can see it run, and fail it.
// A copy of lease.syncFile (internal/lease/operation.go).
var syncFile = (*os.File).Sync

// writeSynced is os.WriteFile with 0600, and the data synced before it returns, so a rename that
// follows can't give a name to a file a crash then empties.
// A copy of lease.writeSynced (internal/lease/operation.go).
func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = syncFile(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
