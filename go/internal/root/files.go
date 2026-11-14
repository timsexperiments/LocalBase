package root

import "os"

// options are the points in this package a test reaches into. Its zero value is how the package
// runs, and the code passes that, so nothing here can be set from outside.
type options struct {
	sync func(*os.File) error // (tests) nil is (*os.File).Sync, which on macOS is F_FULLFSYNC
	uid  func() int           // (tests) nil is os.Getuid
	// owner gives a file another owner (tests); nil keeps the one it has.
	owner func(path string, uid int) int
	// chmod is what closes a directory (tests: a volume that refuses or ignores it); nil is os.Chmod.
	chmod func(string, os.FileMode) error
	// passwd, group and nsswitch are where the local accounts are listed (tests point them
	// elsewhere); "" is /etc/passwd, /etc/group and /etc/nsswitch.conf.
	passwd, group, nsswitch string
}

func (o options) currentUID() int {
	if o.uid != nil {
		return o.uid()
	}
	return os.Getuid()
}

func (o options) ownerOf(path string, uid int) int {
	if o.owner != nil {
		return o.owner(path, uid)
	}
	return uid
}

func (o options) chmodDirectory(dir string, mode os.FileMode) error {
	if o.chmod != nil {
		return o.chmod(dir, mode)
	}
	return os.Chmod(dir, mode)
}

func (o options) etc(path, def string) string {
	if path != "" {
		return path
	}
	return def
}
