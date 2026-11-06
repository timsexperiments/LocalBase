package norm

import "errors"

// The two golang.org/x/text/transform errors Span reports, defined here so the
// package needs nothing outside the standard library.
var (
	errEndOfSpan = errors.New("transform: input and output are not identical")
	errShortSrc  = errors.New("transform: short source buffer")
)
