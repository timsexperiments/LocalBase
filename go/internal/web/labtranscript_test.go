package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

func TestSubtitleFormats(t *testing.T) {
	segments := []labSegment{{Start: 0, End: 1.5, Text: "One"}, {Start: 3661.25, End: 3662, Text: "二"}}
	srt := transcriptSRT(segments)
	if !strings.Contains(srt, "1\n00:00:00,000 --> 00:00:01,500\nOne\n\n") || !strings.Contains(srt, "2\n01:01:01,250 --> 01:01:02,000\n二\n\n") {
		t.Errorf("srt: %q", srt)
	}
	vtt := transcriptVTT(segments)
	if !strings.HasPrefix(vtt, "WEBVTT\n\n") || !strings.Contains(vtt, "00:00:00.000 --> 00:00:01.500\nOne\n\n") {
		t.Errorf("vtt: %q", vtt)
	}
	if got := subtitleTime(-1, ","); got != "00:00:00,000" {
		t.Errorf("negative: %s", got)
	}
}

func TestSegmentWords(t *testing.T) {
	for _, c := range []struct {
		start         float64
		stamp, spoken string
	}{{0, "0:00", "0 seconds"}, {1, "0:01", "1 second"}, {65, "1:05", "1 minute 5 seconds"}, {3661, "1:01:01", "61 minutes 1 second"}} {
		s := labSegment{Start: c.start}
		if s.Stamp() != c.stamp || s.Spoken() != c.spoken {
			t.Errorf("%v: %s, %s", c.start, s.Stamp(), s.Spoken())
		}
	}
	if languageName("en") != "English" || languageName("english") != "English" || languageName("sv") != "Sv" || languageName("") != "" {
		t.Error("language names")
	}
	if transcriptCaption("recording.wav") != "Recording" || transcriptCaption("talk.mp3") != "talk.mp3" {
		t.Error("captions")
	}
}

// A transcript's export is its owner's, and only what the model gave times for.
func TestTranscriptExport(t *testing.T) {
	p := &auth.Principal{Kind: "browser-session", ID: "u", Permissions: []string{"models:read"}}
	u := New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return p }})
	owner := p.OwnerID()
	u.notes.put(owner, "run_a", "talk.mp3")
	u.notes.put(owner, "run_b", "plain.mp3")
	u.notes.update(func() {
		u.notes.items["run_a"].text = "One two"
		u.notes.items["run_a"].segments = []labSegment{{Start: 0, End: 1, Text: "One two"}}
		u.notes.items["run_b"].text = "No times"
	})
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		u.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	for format, want := range map[string]string{"txt": "One two\n", "srt": "00:00:00,000", "vtt": "WEBVTT"} {
		w := get("/app/lab/runs/run_a/export?format=" + format)
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) || !strings.Contains(w.Header().Get("Content-Disposition"), "transcript."+format) {
			t.Errorf("%s: %d %q %v", format, w.Code, w.Body.String(), w.Header())
		}
	}
	if w := get("/app/lab/runs/run_b/export?format=srt"); w.Code != 404 {
		t.Errorf("subtitles with no times: %d", w.Code)
	}
	if w := get("/app/lab/runs/run_b/export?format=txt"); w.Code != 200 {
		t.Errorf("text with no times: %d", w.Code)
	}
	if w := get("/app/lab/runs/nope/export?format=txt"); w.Code != 404 {
		t.Errorf("unknown: %d", w.Code)
	}
}
