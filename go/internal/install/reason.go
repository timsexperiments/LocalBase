package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
)

// The stable reasons an install failed, for a UI to explain it (DESIGN §13.6). The
// error's own sentence stays as it was; these are additive.
const (
	ReasonNetwork          = "network"
	ReasonTimeout          = "timeout"
	ReasonDiskFull         = "disk_full"
	ReasonChecksumMismatch = "checksum_mismatch"
	ReasonGated            = "gated_or_auth_required"
	ReasonNotFound         = "not_found"
	ReasonInterrupted      = "interrupted"
	ReasonUnknown          = "unknown"
)

// HTTPStatusError is a download answered with a non-success status.
type HTTPStatusError struct {
	Code   int
	Status string
}

func (e *HTTPStatusError) Error() string { return fmt.Sprintf("HTTP %d %s", e.Code, e.Status) }

// integrityError: the bytes are not the ones the catalog pins.
type integrityError struct{ message string }

func (e *integrityError) Error() string { return e.message }

// truncatedError: the connection ended before the whole file arrived.
type truncatedError struct{ message string }

func (e *truncatedError) Error() string { return e.message }

// diskSpaceError: the free-space check before downloading failed.
type diskSpaceError struct{ message string }

func (e *diskSpaceError) Error() string { return e.message }

// messageError keeps a chosen sentence while keeping its cause classifiable.
type messageError struct {
	message string
	cause   error
}

func (e *messageError) Error() string { return e.message }
func (e *messageError) Unwrap() error { return e.cause }

// Reason classifies an install error. Anything it does not recognize is unknown.
func Reason(err error) string {
	if err == nil {
		return ""
	}
	var status *HTTPStatusError
	var integrity *integrityError
	var truncated *truncatedError
	var space *diskSpaceError
	var netErr net.Error
	var urlErr *url.Error
	var local *os.PathError
	switch {
	case errors.Is(err, context.Canceled):
		return ReasonInterrupted
	case errors.Is(err, context.DeadlineExceeded):
		return ReasonTimeout
	case errors.As(err, &space), errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return ReasonDiskFull
	case errors.As(err, &status):
		switch {
		case status.Code == http.StatusUnauthorized, status.Code == http.StatusForbidden:
			return ReasonGated
		case status.Code == http.StatusNotFound, status.Code == http.StatusGone:
			return ReasonNotFound
		case status.Code == http.StatusRequestTimeout, status.Code == http.StatusGatewayTimeout:
			return ReasonTimeout
		case status.Code == http.StatusTooManyRequests, status.Code >= 500:
			return ReasonNetwork
		}
		return ReasonUnknown
	case errors.As(err, &integrity):
		return ReasonChecksumMismatch
	case errors.As(err, &local):
		return ReasonUnknown // a local file error also satisfies net.Error
	case errors.As(err, &netErr) && netErr.Timeout():
		return ReasonTimeout
	case errors.As(err, &truncated), errors.Is(err, io.ErrUnexpectedEOF), errors.As(err, &netErr), errors.As(err, &urlErr):
		return ReasonNetwork
	}
	return ReasonUnknown
}
