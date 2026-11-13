package root

import "os"

// options are the points in this package a test reaches into. Its zero value is how the package
// runs, and the code passes that, so nothing here can be set from outside.
type options struct {
	sync func(*os.File) error // (tests) nil is (*os.File).Sync, which on macOS is F_FULLFSYNC
}

// fsync is a copy of lease's (internal/lease/operation.go).
func (o options) fsync(f *os.File) error {
	if o.sync != nil {
		return o.sync(f)
	}
	return f.Sync()
}

// writeSynced is os.WriteFile with 0600, and the data synced before it returns, so a rename that
// follows can't give a name to a file a crash then empties.
// A copy of lease.writeSynced (internal/lease/operation.go).
func writeSynced(path string, data []byte, o options) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = o.fsync(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
