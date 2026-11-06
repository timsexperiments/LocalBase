package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// The Speech studio (DESIGN §22.19): a script longer than one request is cut at
// sentence boundaries, made one part at a time on the gateway as the signed-in
// person, and joined into one file. The job lives here rather than in the
// browser, so a reload or a suspended phone loses nothing.

const (
	speechScriptLimit = 5000 // runes in a script
	// speechPartLimit stays under the API's 256 runes, and further under the
	// frame cap a part full of digits can reach (DESIGN §24).
	speechPartLimit = 200
	// speechPartFrames budgets a part by how long it takes to say (partFrames),
	// under the runtime's cap of 256 frames. A list of numbers comes to about 85
	// characters. That lowers the chance of a cut and does not remove it: the model's
	// length varies, so the API's cut flag stays the real signal.
	speechPartFrames = 200
	speechJobTimeout = 30 * time.Minute
	// speechPartPause is the gap between joined parts, after each side's own
	// silence is trimmed. It is about the middle of the gaps the real model leaves
	// between sentences inside a part (2026-11-02); checked by analysis, not by ear.
	speechPartPause  = 350 * time.Millisecond
	speechEdgeQuiet  = 64                    // a sample this small is silence at a part's edge
	speechEdgeKeep   = 40 * time.Millisecond // of that silence, kept each side
	speechEdgeFade   = 10 * time.Millisecond // so an edge the model cut does not click
	speechPeaks      = 64
	speechBarMax     = 24 // the waveform's height
	previewCacheSize = 32
	previewSentence  = "Hello. This is how this voice sounds."
)

// sentenceEnd reports a rune that ends a sentence.
func sentenceEnd(c rune) bool { return strings.ContainsRune(".!?…。！？\n", c) }

// partFrames estimates the frames a part takes to say. Measured on the real model
// (2026-11-01 and 2026-11-02, DESIGN §24): prose takes about 0.7 a character, and
// a list of numbers up to about 2.8. Spaces in that list count as prose, so digits
// and commas are set a little under it, to land near 85 characters a part.
func partFrames(s string) int {
	tenths := 0
	for _, c := range s {
		if unicode.IsDigit(c) || c == ',' {
			tenths += 27
		} else {
			tenths += 7
		}
	}
	return tenths / 10
}

// fits says whether text can be one part.
func fits(s string, limit int) bool {
	return utf8.RuneCountInString(s) <= limit && partFrames(s) <= speechPartFrames
}

// splitScript cuts text into parts of at most limit runes and speechPartFrames: at
// sentence ends where it can, then at a comma or semicolon, then at a space, and
// only for a run with no space at all, in the middle.
func splitScript(text string, limit int) []string {
	var sentences []string
	var cur []rune
	flush := func() {
		if s := strings.TrimSpace(string(cur)); s != "" {
			sentences = append(sentences, s)
		}
		cur = nil
	}
	runes := []rune(text)
	for i, c := range runes {
		cur = append(cur, c)
		if sentenceEnd(c) && (i+1 == len(runes) || unicode.IsSpace(runes[i+1]) || c >= 0x3000) {
			flush()
		}
	}
	flush()

	var parts []string
	var part string
	add := func(s string) {
		if part != "" && !fits(part+" "+s, limit) {
			parts = append(parts, part)
			part = ""
		}
		if part == "" {
			part = s
		} else {
			part += " " + s
		}
	}
	for _, s := range sentences {
		for !fits(s, limit) {
			head, rest := cutLong([]rune(s), limit)
			add(head)
			s = rest
		}
		add(s)
	}
	if part != "" {
		parts = append(parts, part)
	}
	return parts
}

// cutLong takes up to limit runes from a sentence too long for one part.
func cutLong(r []rune, limit int) (string, string) {
	// As many runes as fit the part's budget.
	for limit > 1 && !fits(string(r[:min(limit, len(r))]), limit) {
		limit--
	}
	limit = min(limit, len(r))
	at := -1
	for i := limit - 1; i > limit/2 && at < 0; i-- {
		if strings.ContainsRune(",;:，；：", r[i]) {
			at = i + 1
		}
	}
	for i := limit - 1; i > limit/2 && at < 0; i-- {
		if unicode.IsSpace(r[i]) {
			at = i + 1
		}
	}
	if at < 0 {
		at = limit
	}
	return strings.TrimSpace(string(r[:at])), strings.TrimSpace(string(r[at:]))
}

// wavFormat is what a joined file needs to agree on.
type wavFormat struct {
	Channels, Bits uint16
	Rate           uint32
}

// parseWAV finds a PCM WAV's format and samples.
func parseWAV(b []byte) (wavFormat, []byte, error) {
	var f wavFormat
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return f, nil, errors.New("not a WAV file")
	}
	for at := 12; at+8 <= len(b); {
		id, size := string(b[at:at+4]), int(binary.LittleEndian.Uint32(b[at+4:]))
		body := at + 8
		switch id {
		case "fmt ":
			if size < 16 || body+16 > len(b) {
				return f, nil, errors.New("a short WAV header")
			}
			f.Channels = binary.LittleEndian.Uint16(b[body+2:])
			f.Rate = binary.LittleEndian.Uint32(b[body+4:])
			f.Bits = binary.LittleEndian.Uint16(b[body+14:])
		case "data":
			if f.Rate == 0 {
				return f, nil, errors.New("WAV samples before a format")
			}
			return f, b[body:min(body+size, len(b))], nil
		}
		at = body + size + size%2
	}
	return f, nil, errors.New("a WAV file without samples")
}

// joinWAV joins parts of one format, with a short pause between them.
func joinWAV(parts [][]byte) ([]byte, wavFormat, []byte, error) {
	var first wavFormat
	var pcm bytes.Buffer
	for i, p := range parts {
		f, samples, err := parseWAV(p)
		if err != nil {
			return nil, first, nil, err
		}
		if i == 0 {
			first = f
		} else if f != first {
			return nil, first, nil, errors.New("the parts have different formats")
		}
		bytesPerSecond := int(f.Rate) * int(f.Channels) * int(f.Bits) / 8
		samples = shapeEdges(samples, f, i > 0, i < len(parts)-1)
		if i > 0 {
			pause := int(speechPartPause.Seconds()*float64(bytesPerSecond)) &^ 1
			pcm.Write(make([]byte, pause))
		}
		pcm.Write(samples)
	}
	h := make([]byte, 44)
	copy(h, "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+pcm.Len()))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], first.Channels)
	binary.LittleEndian.PutUint32(h[24:], first.Rate)
	binary.LittleEndian.PutUint32(h[28:], first.Rate*uint32(first.Channels)*uint32(first.Bits)/8)
	binary.LittleEndian.PutUint16(h[32:], first.Channels*first.Bits/8)
	binary.LittleEndian.PutUint16(h[34:], first.Bits)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(pcm.Len()))
	return append(h, pcm.Bytes()...), first, pcm.Bytes(), nil
}

// shapeEdges prepares a part's joined edges: its own silence is trimmed, so every
// gap is the same length, and the last moment fades, so an edge the model cut does
// not click. Only 16-bit audio is touched, on a copy.
func shapeEdges(samples []byte, f wavFormat, head, tail bool) []byte {
	if f.Bits != 16 || f.Channels == 0 || f.Rate == 0 {
		return samples
	}
	frame := 2 * int(f.Channels)
	count := func(d time.Duration) int { return int(d.Seconds()*float64(f.Rate)) * frame }
	loud := func(at int) bool {
		v := int(int16(binary.LittleEndian.Uint16(samples[at:])))
		return v > speechEdgeQuiet || v < -speechEdgeQuiet
	}
	from, to := 0, len(samples)&^1
	if head {
		for from < to && !loud(from) {
			from += 2
		}
		from = max(0, from-count(speechEdgeKeep)) / frame * frame
	}
	if tail {
		for to > from && !loud(to-2) {
			to -= 2
		}
		to = min(len(samples)&^1, to+count(speechEdgeKeep)) / frame * frame
	}
	out := append([]byte(nil), samples[from:to]...)
	if tail {
		fade := min(count(speechEdgeFade), len(out)) &^ 1
		for i := 0; i < fade; i += 2 {
			at := len(out) - fade + i
			v := float64(int16(binary.LittleEndian.Uint16(out[at:])))
			binary.LittleEndian.PutUint16(out[at:], uint16(int16(v*float64(fade-i)/float64(fade))))
		}
	}
	return out
}

// waveBar is one bar of a drawn waveform.
type waveBar struct{ X, Y, H int }

// wavePeaks draws 16-bit samples as bars: each bar is the loudest sample in its
// share of the file, scaled to the file's own loudest.
func wavePeaks(pcm []byte) []waveBar {
	samples := len(pcm) / 2
	if samples == 0 {
		return nil
	}
	levels := make([]int, speechPeaks)
	top := 1
	for i := range levels {
		for s := i * samples / speechPeaks; s < (i+1)*samples/speechPeaks; s++ {
			v := int(int16(binary.LittleEndian.Uint16(pcm[s*2:])))
			if v < 0 {
				v = -v
			}
			levels[i] = max(levels[i], v)
		}
		top = max(top, levels[i])
	}
	bars := make([]waveBar, speechPeaks)
	for i, level := range levels {
		h := max(2, level*speechBarMax/top)
		bars[i] = waveBar{X: i * 3, Y: (speechBarMax - h) / 2, H: h}
	}
	return bars
}

// durationWords says a length in words.
func durationWords(d time.Duration) string {
	seconds := int(d.Round(time.Second).Seconds())
	switch {
	case seconds < 1:
		return "Under a second"
	case seconds < 60:
		return fmt.Sprintf("%d s", seconds)
	}
	return fmt.Sprintf("%d min %d s", seconds/60, seconds%60)
}

// speechJob is one script being made. notes.mu guards it.
type speechJob struct {
	Model, Voice, VoiceName string
	Parts                   []string
	Done, Cut               int
	Status                  string // active | completed | stopped | failed
	Error                   string
	Result                  string
	Bars                    []waveBar
	Length                  string
	cancel                  context.CancelFunc
}

// update changes a job under the lock.
func (n *labNotes) update(fn func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	fn()
}

func (n *labNotes) speechJobs(owner string) map[string]*labNote {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := map[string]*labNote{}
	for id, v := range n.items {
		if v.owner == owner && v.speech != nil && time.Since(v.at) < resultTTL+resultTTL {
			out[id] = v
		}
	}
	return out
}

// labSpeech starts a script.
func (u *UI) labSpeech(w http.ResponseWriter, r *http.Request) {
	u.startSpeech(w, r, r.PostFormValue("input"), r.PostFormValue("model"), r.PostFormValue("voice"))
}

func (u *UI) labSpeechAgain(w http.ResponseWriter, r *http.Request) {
	note := u.notes.get(u.caller(r).OwnerID(), r.PathValue("id"))
	if note == nil || note.speech == nil {
		u.labOutcome(w, r, map[string]any{"Error": "This script is gone. Write it again."})
		return
	}
	u.startSpeech(w, r, note.prompt, note.speech.Model, note.speech.Voice)
}

func (u *UI) startSpeech(w http.ResponseWriter, r *http.Request, script, model, voice string) {
	script = strings.TrimSpace(script)
	switch n := utf8.RuneCountInString(script); {
	case n == 0:
		u.labOutcome(w, r, map[string]any{"Error": "Write a script first."})
		return
	case n > speechScriptLimit:
		u.labOutcome(w, r, map[string]any{"Error": fmt.Sprintf("This script has %d characters, and the limit is %d. Nothing was cut: shorten it and send it again.", n, speechScriptLimit)})
		return
	}
	if u.d.Call == nil {
		u.labOutcome(w, r, map[string]any{"Error": http.StatusText(http.StatusServiceUnavailable)})
		return
	}
	p := u.caller(r)
	owner := p.OwnerID()
	for _, note := range u.notes.speechJobs(owner) {
		active := false
		u.notes.update(func() { active = note.speech.Status == "active" })
		if active {
			u.labOutcome(w, r, map[string]any{"Error": "One script at a time. Stop the one that’s running, or send this when it finishes."})
			return
		}
	}
	name := voice
	for _, v := range u.labVoices(r.Context(), model) {
		if v.ID == voice {
			name = v.Name
		}
	}
	b := make([]byte, 16)
	rand.Read(b)
	id := "sp" + hex.EncodeToString(b)
	ctx, cancel := context.WithTimeout(context.Background(), speechJobTimeout)
	job := &speechJob{Model: model, Voice: voice, VoiceName: name, Parts: splitScript(script, speechPartLimit), Status: "active", cancel: cancel}
	u.notes.put(owner, id, script)
	u.notes.update(func() { u.notes.items[id].speech = job })
	go u.runSpeech(ctx, p, id, job)
	w.Header().Set("HX-Retarget", "#lab-items")
	w.Header().Set("HX-Reswap", "afterbegin")
	u.fragment(w, r, "lab", "lab-item", u.speechItem(id, u.notes.get(owner, id)))
}

// runSpeech makes the parts one at a time and joins what it got.
func (u *UI) runSpeech(ctx context.Context, p *auth.Principal, id string, job *speechJob) {
	defer job.cancel()
	var made [][]byte
	var failure string
	stopped := false
	for _, text := range job.Parts {
		if ctx.Err() != nil {
			stopped = true
			break
		}
		body, _ := json.Marshal(map[string]any{"model": job.Model, "input": text, "voice": job.Voice, "response_format": "wav"})
		res := u.d.Call(ctx, p, "POST", "/v1/audio/speech", "application/json", body)
		if ctx.Err() != nil {
			stopped = true
			break
		}
		if msg := res.apiError(); msg != "" {
			failure = msg
			break
		}
		made = append(made, res.Body)
		cut := res.Header.Get("X-Localbase-Audio-Truncated") == "true"
		u.notes.update(func() {
			job.Done++
			if cut {
				job.Cut++
			}
		})
	}
	audio, length, bars, joinErr := speechResult(made)
	result := ""
	if len(audio) > 0 {
		result = u.results.put(p.OwnerID(), "audio/wav", audio)
	}
	u.notes.update(func() {
		job.Result, job.Length, job.Bars = result, length, bars
		switch {
		case stopped:
			job.Status = "stopped"
		case failure != "":
			job.Status, job.Error = "failed", failure
		case joinErr != nil:
			job.Status, job.Error = "failed", "The parts could not be joined. Try again."
		default:
			job.Status = "completed"
		}
	})
}

// speechResult is the file for the parts made so far. One part stays as it came.
func speechResult(made [][]byte) (audio []byte, length string, bars []waveBar, err error) {
	if len(made) == 0 {
		return nil, "", nil, nil
	}
	var format wavFormat
	var pcm []byte
	if audio, format, pcm, err = joinWAV(made); err != nil {
		if len(made) == 1 {
			return made[0], "", nil, nil
		}
		return nil, "", nil, err
	}
	if len(made) == 1 {
		audio = made[0]
	}
	if bytesPerSecond := int(format.Rate) * int(format.Channels) * int(format.Bits) / 8; bytesPerSecond > 0 {
		length = durationWords(time.Duration(float64(len(pcm)) / float64(bytesPerSecond) * float64(time.Second)))
	}
	if format.Bits == 16 {
		bars = wavePeaks(pcm)
	}
	return audio, length, bars, nil
}

func (u *UI) labSpeechStatus(w http.ResponseWriter, r *http.Request) {
	u.speechRender(w, r)
}

func (u *UI) speechRender(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	note := u.notes.get(u.caller(r).OwnerID(), id)
	if note == nil || note.speech == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		return
	}
	u.fragment(w, r, "lab", "lab-item", u.speechItem(id, note))
}

// labSpeechStop stops after the part in hand; what was made stays.
func (u *UI) labSpeechStop(w http.ResponseWriter, r *http.Request) {
	if note := u.notes.get(u.caller(r).OwnerID(), r.PathValue("id")); note != nil && note.speech != nil {
		note.speech.cancel()
		u.notes.update(func() {
			if note.speech.Status == "active" {
				note.speech.Status = "stopping"
			}
		})
	}
	u.speechRender(w, r)
}

func (u *UI) labSpeechRemove(w http.ResponseWriter, r *http.Request) {
	owner := u.caller(r).OwnerID()
	if note := u.notes.get(owner, r.PathValue("id")); note != nil && note.speech != nil {
		note.speech.cancel()
	}
	u.notes.drop(owner, r.PathValue("id"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

// speechItems are the person's scripts for the tray.
func (u *UI) speechItems(r *http.Request) []labItemView {
	owner := u.caller(r).OwnerID()
	var out []labItemView
	for id, note := range u.notes.speechJobs(owner) {
		v := u.speechItem(id, note)
		// Its file has expired: it leaves the tray.
		if !v.Active && v.Audio == "" && v.Status == "completed" {
			u.notes.drop(owner, id)
			continue
		}
		out = append(out, v)
	}
	return out
}

// speechItem describes a script for the tray.
func (u *UI) speechItem(id string, note *labNote) labItemView {
	v := labItemView{ID: id, Tool: "speech", Caption: note.caption, Prompt: note.prompt, At: note.at, Ago: agoText(note.at, time.Now()), Again: true}
	u.notes.update(func() {
		job := note.speech
		total := len(job.Parts)
		v.Status, v.Model, v.VoiceName, v.Part, v.Parts, v.Bars, v.Length = job.Status, job.Model, job.VoiceName, job.Done, total, job.Bars, job.Length
		v.Active = job.Status == "active" || job.Status == "stopping"
		switch job.Status {
		case "active":
			v.Message = "Speaking…"
			if total > 1 {
				v.Message = fmt.Sprintf("Making part %d of %d…", min(job.Done+1, total), total)
			}
		case "stopping":
			v.Message = "Stopping…"
		case "stopped":
			v.Message = "Stopped. Nothing was saved."
			if job.Done > 0 {
				v.Message = fmt.Sprintf("Stopped after part %d of %d. Here is what was made.", job.Done, total)
			}
		case "failed":
			v.Message = job.Error
			if job.Done > 0 {
				v.Message = fmt.Sprintf("This stopped at part %d of %d: %s Here is what was made.", job.Done+1, total, job.Error)
			}
		case "completed":
			if job.Cut > 0 {
				v.Message = "The model cut part of this short, so some words may be missing. Listen through it, and try shorter sentences."
			}
		}
		if job.Result != "" {
			v.Audio = "/app/lab/results/" + job.Result
		}
	})
	if v.Audio != "" && u.results.get(note.owner, strings.TrimPrefix(v.Audio, "/app/lab/results/")) == nil {
		v.Audio = ""
	}
	v.Announce = v.Message
	if v.Status == "completed" && v.Message == "" {
		v.Announce = "The speech is ready."
	}
	// Each part is news only every so often; the bar shows the rest.
	if v.Active && v.Parts > 1 {
		v.Announce = "Making the speech."
	}
	return v
}

// labVoicePreview speaks a short sentence in a voice, once per model and voice.
func (u *UI) labVoicePreview(w http.ResponseWriter, r *http.Request) {
	// A preview can load a model, which can move another out of memory, so a page elsewhere
	// can't ask for one, not even under the same domain.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	model, voice := r.URL.Query().Get("model"), r.URL.Query().Get("voice")
	key := model + "\x00" + voice
	u.previewMu.Lock()
	audio := u.previews[key]
	u.previewMu.Unlock()
	if audio == nil {
		body, _ := json.Marshal(map[string]any{"model": model, "input": previewSentence, "voice": voice, "response_format": "wav"})
		res := u.call(r, "POST", "/v1/audio/speech", "application/json", body)
		if msg := res.apiError(); msg != "" {
			http.Error(w, msg, http.StatusBadGateway)
			return
		}
		audio = res.Body
		u.previewMu.Lock()
		if u.previews == nil || len(u.previews) >= previewCacheSize {
			u.previews = map[string][]byte{}
		}
		u.previews[key] = audio
		u.previewMu.Unlock()
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(audio))
}

// previewURL plays a sample of a voice.
func previewURL(model, voice string) string {
	return "/app/lab/voices/preview?model=" + url.QueryEscape(model) + "&voice=" + url.QueryEscape(voice)
}
