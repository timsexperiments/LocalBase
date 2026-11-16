package audit

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// A class is only ever from the short list.
func TestAuditFailureClass(t *testing.T) {
	for want, err := range map[string]error{
		"permission-denied": &fs.PathError{Op: "open", Path: "x", Err: syscall.EACCES},
		"disk-full":         &fs.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC},
		"missing":           fs.ErrNotExist,
		"not-a-file":        &fs.PathError{Op: "mkdir", Path: "x", Err: syscall.ENOTDIR},
		"write-failed":      errors.New("PLANTED_anything_else"),
	} {
		if got := failureClass(err); got != want {
			t.Errorf("%v: %q, want %q", err, got, want)
		}
	}
}

// Whatever an error's text holds, it isn't repeated: not in the warning, and not in the result. The
// class alone, from the short list, is. (Was cmd/localbase's TestAnAuditWarningNeverRepeatsTheError,
// which ran keys create with this seam set; the seam is the Log's openSink now.)
func TestARecordThatCannotBeWrittenNeverRepeatsTheError(t *testing.T) {
	r, err := root.Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	const planted = "PLANTED_secret_in_an_error_7d1e0c"
	var warnings bytes.Buffer
	l := New(r, &warnings)
	l.openSink = func(string) (*obs.Sink, error) {
		return nil, &fs.PathError{Op: "open", Path: "/" + planted, Err: fs.ErrPermission}
	}
	l.Change("api-key", "create", "api-key", "key_1")
	if !strings.Contains(warnings.String(), "could not be written: permission-denied") {
		t.Errorf("no class in the warning: %q", warnings.String())
	}
	o := l.Outcome()
	res := l.WithResult(map[string]any{})
	if o.AuditWritten == nil || *o.AuditWritten || o.AuditError != "permission-denied" || res["auditError"] != "permission-denied" || res["auditWritten"] != false {
		t.Errorf("the result: %+v, %v", o, res)
	}
	if all := warnings.String() + fmt.Sprint(o, res); strings.Contains(all, planted) {
		t.Errorf("the error's text is repeated: %q", all)
	}
}
