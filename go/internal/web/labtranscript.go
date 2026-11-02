package web

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
)

// The Transcribe studio (DESIGN §22.20): a recording or a file becomes a detached
// run, and the transcript comes back with the times of each stretch, which seek the
// audio and make the subtitle exports.

// labSegment is one stretch of a transcript.
type labSegment struct {
	Start, End float64
	Text       string
}

// Stamp is where a stretch starts, as a person reads it: 1:05.
func (s labSegment) Stamp() string {
	total := int(s.Start)
	if total >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", total/3600, total/60%60, total%60)
	}
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}

// Spoken is the same for someone listening: "1 minute 5 seconds".
func (s labSegment) Spoken() string {
	total := int(s.Start)
	unit := func(n int, name string) string {
		if n == 1 {
			return "1 " + name
		}
		return fmt.Sprintf("%d %ss", n, name)
	}
	if total < 60 {
		return unit(total, "second")
	}
	return unit(total/60, "minute") + " " + unit(total%60, "second")
}

// transcriptLanguages are offered by name; Detect leaves it to the model.
var transcriptLanguages = []struct{ Code, Name string }{
	{"en", "English"}, {"es", "Spanish"}, {"fr", "French"}, {"de", "German"}, {"it", "Italian"}, {"pt", "Portuguese"},
	{"nl", "Dutch"}, {"ru", "Russian"}, {"zh", "Chinese"}, {"ja", "Japanese"}, {"ko", "Korean"}, {"ar", "Arabic"}, {"hi", "Hindi"},
}

// languageName names a language the model reports, by code or by its own name.
func languageName(code string) string {
	code = strings.TrimSpace(code)
	for _, l := range transcriptLanguages {
		if strings.EqualFold(l.Code, code) || strings.EqualFold(l.Name, code) {
			return l.Name
		}
	}
	if code == "" {
		return ""
	}
	return strings.ToUpper(code[:1]) + code[1:]
}

var englishOnlyRe = regexp.MustCompile(`(?i)(^|[-_.])en($|[-_.])`)

// englishOnly says a transcription model hears English only. The catalog marks the
// models that take other languages; anything else goes by the model's name, as the
// Hub listing does (hub/entry.go), so an oddly named import can be misread.
func (u *UI) englishOnly(id string) bool {
	if u.d.Catalog != nil {
		if s := u.d.Catalog.ByID(id); s != nil {
			for _, c := range s.Capabilities {
				if c.ID == "multilingual" {
					return false
				}
			}
		}
	}
	return englishOnlyRe.MatchString(id)
}

// sttModels are the selected transcription models, each saying if it hears English only.
func (u *UI) sttModels() []labModel {
	models := u.labModels("stt")
	for i := range models {
		models[i].EnglishOnly = u.englishOnly(models[i].ID)
	}
	return models
}

// transcriptCaption is the tray's name for it: a file's name, or that it was recorded.
func transcriptCaption(file string) string {
	if file == "" || strings.HasPrefix(file, "recording") {
		return "Recording"
	}
	return file
}

// subtitleTime formats seconds for SRT (a comma) or VTT (a dot).
func subtitleTime(seconds float64, mark string) string {
	ms := int64(math.Round(math.Max(seconds, 0) * 1000))
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", ms/3600000, ms/60000%60, ms/1000%60, mark, ms%1000)
}

// transcriptSRT writes numbered subtitles.
func transcriptSRT(segments []labSegment) string {
	var b strings.Builder
	for i, s := range segments {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, subtitleTime(s.Start, ","), subtitleTime(s.End, ","), s.Text)
	}
	return b.String()
}

// transcriptVTT writes web subtitles.
func transcriptVTT(segments []labSegment) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, s := range segments {
		fmt.Fprintf(&b, "%s --> %s\n%s\n\n", subtitleTime(s.Start, "."), subtitleTime(s.End, "."), s.Text)
	}
	return b.String()
}

// labTranscriptExport downloads a transcript as text or subtitles.
func (u *UI) labTranscriptExport(w http.ResponseWriter, r *http.Request) {
	note := u.notes.get(u.caller(r).OwnerID(), r.PathValue("id"))
	if note == nil || note.text == "" {
		http.NotFound(w, r)
		return
	}
	var body, ext string
	u.notes.collecting.Lock()
	segments := note.segments
	text := note.text
	u.notes.collecting.Unlock()
	switch format := r.URL.Query().Get("format"); {
	case format == "srt" && len(segments) > 0:
		body, ext = transcriptSRT(segments), "srt"
	case format == "vtt" && len(segments) > 0:
		body, ext = transcriptVTT(segments), "vtt"
	case format == "txt":
		body, ext = text+"\n", "txt"
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="transcript.`+ext+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Write([]byte(body))
}
