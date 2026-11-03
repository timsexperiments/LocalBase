package store

// What a ReadError was reading, for the warning a listing carries.
const (
	ReadPeaks        = "peak-read-failed"
	ReadCapabilities = "capability-read-failed"
	ReadLibrary      = "library-read-failed"
)

// ReadError is a read something needed and couldn't do, naming which, so that nothing goes on as if
// there were nothing there.
type ReadError struct {
	Step string
	Err  error
	Kind string // one of the Read constants, when a listing reports it
}

func (e *ReadError) Error() string { return e.Step + ": " + e.Err.Error() }
func (e *ReadError) Unwrap() error { return e.Err }

// Reason is why, in words that are safe to show: never the database's own.
func (e *ReadError) Reason() string {
	if IsBusy(e.Err) {
		return "the database is busy"
	}
	return "the read failed"
}
