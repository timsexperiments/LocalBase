package store

// ReadError is a read something needed and couldn't do, naming which, so that nothing goes on as if
// there were nothing there.
type ReadError struct {
	Step string
	Err  error
}

func (e *ReadError) Error() string { return e.Step + ": " + e.Err.Error() }
func (e *ReadError) Unwrap() error { return e.Err }
