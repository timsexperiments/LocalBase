package httpx

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// deadlineWriter records what an answer does to its connection.
type deadlineWriter struct {
	header    http.Header
	deadlines []time.Time
	pieces    []int
	flushes   int
	armed     []int // pieces written before each deadline
	failOn    int   // the piece that fails, from 1; 0 never
	partial   int   // how much of it was sent
}

func (d *deadlineWriter) Header() http.Header { return d.header }
func (d *deadlineWriter) WriteHeader(int)     {}
func (d *deadlineWriter) Flush()              { d.flushes++ }
func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	d.armed = append(d.armed, len(d.pieces))
	return nil
}
func (d *deadlineWriter) Write(b []byte) (int, error) {
	d.pieces = append(d.pieces, len(b))
	if d.failOn != 0 && len(d.pieces) == d.failOn {
		return d.partial, errors.New("the connection is gone")
	}
	return len(b), nil
}

func TestEachPieceOfAnAnswerHasItsOwnDeadline(t *testing.T) {
	d := &deadlineWriter{header: http.Header{}}
	rec := &Recorder{ResponseWriter: d}
	rec.SendsWithin(time.Minute)
	const size = WritePiece*3 + 10
	start := time.Now()
	n, err := rec.Write(make([]byte, size))
	if n != size || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if len(d.pieces) != 4 {
		t.Fatalf("%d pieces: %v", len(d.pieces), d.pieces)
	}
	total := 0
	for i, p := range d.pieces {
		total += p
		if p > WritePiece {
			t.Errorf("piece %d is %d bytes", i, p)
		}
		// Set just before its piece, and far enough off.
		if d.armed[i] != i {
			t.Errorf("piece %d: deadlines were set before pieces %v", i, d.armed)
		}
		if left := d.deadlines[i].Sub(start); left < 59*time.Second || left > time.Minute+time.Second {
			t.Errorf("piece %d has %v", i, left)
		}
	}
	if total != size {
		t.Errorf("%d of %d bytes", total, size)
	}
}

func TestAFailedPieceEndsTheAnswer(t *testing.T) {
	d := &deadlineWriter{header: http.Header{}, failOn: 2, partial: 7}
	rec := &Recorder{ResponseWriter: d}
	rec.SendsWithin(time.Minute)
	n, err := rec.Write(make([]byte, WritePiece*4))
	if err == nil || n != WritePiece+7 {
		t.Errorf("%d %v", n, err)
	}
	if len(d.pieces) != 2 {
		t.Errorf("%d pieces after the failure", len(d.pieces))
	}
}

func TestAFlushHasItsOwnDeadline(t *testing.T) {
	d := &deadlineWriter{header: http.Header{}}
	rec := &Recorder{ResponseWriter: d}
	rec.SendsWithin(time.Minute)
	before := len(d.deadlines)
	rec.Flush()
	if len(d.deadlines) != before+1 || d.deadlines[before].IsZero() || d.flushes != 1 {
		t.Errorf("%v, %d flushes", d.deadlines, d.flushes)
	}
}

// Nothing says how long to wait, in a recorder made for something inside the gateway.
func TestARecorderWithNoLimitOnlyWrites(t *testing.T) {
	d := &deadlineWriter{header: http.Header{}}
	rec := &Recorder{ResponseWriter: d}
	if n, err := rec.Write(make([]byte, WritePiece*2)); n != WritePiece*2 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	rec.Flush()
	if len(d.deadlines) != 0 || len(d.pieces) != 1 {
		t.Errorf("%d deadlines, %d pieces", len(d.deadlines), len(d.pieces))
	}
}

func TestTheWriteLimitIsTheStatedOne(t *testing.T) {
	if DefaultLimits.WriteStall != time.Minute || WritePiece != 64<<10 {
		t.Errorf("%v, %d", DefaultLimits.WriteStall, WritePiece)
	}
	if got := (Limits{}).WithDefaults().WriteStall; got != time.Minute {
		t.Errorf("a gateway that sets none gets %v", got)
	}
	if got := (Limits{WriteStall: time.Second}).WithDefaults().WriteStall; got != time.Second {
		t.Errorf("a gateway that sets one gets %v", got)
	}
}

// What a request came to, for the log and for usage. A status goes out before the answer does.
func TestAFailedAnswerIsNotASuccess(t *testing.T) {
	for _, c := range []struct {
		status int
		failed bool
		want   int
	}{
		{0, false, 200},
		{200, false, 200},
		{204, false, 204},
		{0, true, 499},
		{200, true, 499},
		{302, true, 499},
		{404, true, 404},
		{500, true, 500},
	} {
		r := &Recorder{Status: c.status, SendFailed: c.failed}
		if got := r.FinalStatus(); got != c.want {
			t.Errorf("status %d, failed %v: %d, want %d", c.status, c.failed, got, c.want)
		}
	}
}

func TestOnlyAFailedPieceCountsAsFailed(t *testing.T) {
	d := &deadlineWriter{header: http.Header{}}
	rec := &Recorder{ResponseWriter: d}
	rec.SendsWithin(time.Minute)
	rec.Write(make([]byte, WritePiece*2))
	if rec.SendFailed {
		t.Error("a whole answer counted as failed")
	}
	d = &deadlineWriter{header: http.Header{}, failOn: 2, partial: 7}
	rec = &Recorder{ResponseWriter: d}
	rec.SendsWithin(time.Minute)
	rec.Write(make([]byte, WritePiece*2))
	if !rec.SendFailed {
		t.Error("an answer cut short counted as whole")
	}
}
