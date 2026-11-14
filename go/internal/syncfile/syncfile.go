// Package syncfile writes a file and syncs it before it returns, so that a rename that follows can't give
// a name to a file a crash then empties.
//
// It promises only that. It opens the path it is given as it is: it follows a link there, truncates what is
// there, and checks no owner. Its callers write a temporary name, in a directory only they can write, and
// then rename it. For a path that a link or another user's file could be planted at, use safefile, which
// refuses those.
package syncfile

import "os"

// Write is os.WriteFile with mode 0600, and the data synced before it returns. sync is how it syncs: nil is
// (*os.File).Sync, which on macOS is F_FULLFSYNC. A test passes its own, to see it run and to fail it.
func Write(path string, data []byte, sync func(*os.File) error) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		if sync == nil {
			sync = (*os.File).Sync
		}
		err = sync(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
