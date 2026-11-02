package web

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

func testWAV(samples ...int16) []byte {
	pcm := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(s))
	}
	h := make([]byte, 44)
	copy(h, "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(pcm)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], 24000)
	binary.LittleEndian.PutUint32(h[28:], 48000)
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(pcm)))
	return append(h, pcm...)
}

func TestSplitScript(t *testing.T) {
	// Sentences stay whole, and join up to the limit.
	if got := splitScript("One. Two! Three?", 10); strings.Join(got, "|") != "One. Two!|Three?" {
		t.Errorf("sentences: %q", got)
	}
	// A decimal point does not end a sentence.
	if got := splitScript("It costs 3.50 today.", 200); len(got) != 1 {
		t.Errorf("decimal: %q", got)
	}
	// Chinese sentences end without a space.
	if got := splitScript("你好。今天很好。", 4); strings.Join(got, "|") != "你好。|今天很好|。" && len(got) < 2 {
		t.Errorf("no spaces: %q", got)
	}
	// A long sentence cuts at a comma, then a space, then wherever it must.
	long := strings.Repeat("word ", 30) + "end, " + strings.Repeat("more ", 30)
	for _, part := range splitScript(long, 50) {
		if n := utf8.RuneCountInString(part); n > 50 || n == 0 {
			t.Errorf("part of %d: %q", n, part)
		}
		if strings.HasPrefix(part, "ord") {
			t.Errorf("cut inside a word: %q", part)
		}
	}
	for _, part := range splitScript(strings.Repeat("x", 120), 50) {
		if utf8.RuneCountInString(part) > 50 {
			t.Errorf("no spaces: %q", part)
		}
	}
	// Numbers take longer to say: a list is cut well before the character limit.
	list := make([]string, 80)
	for i := range list {
		list[i] = strconv.Itoa(1000 + i)
	}
	numbers := strings.Join(list, ", ")
	got := splitScript(numbers, speechPartLimit)
	for _, part := range got {
		if partFrames(part) > speechPartFrames {
			t.Errorf("%d frames: %q", partFrames(part), part)
		}
	}
	if strings.Join(got, " ") != numbers {
		t.Errorf("lost numbers: %q", got)
	}
	for _, part := range got {
		if n := utf8.RuneCountInString(part); n > 90 {
			t.Errorf("a list of numbers in %d characters: %q", n, part)
		}
	}
	// Nothing is lost.
	text := "First sentence here. Second, with a comma, and more words after it! Third?\nFourth line."
	joined := strings.Join(splitScript(text, 30), " ")
	if strings.Join(strings.Fields(joined), " ") != strings.Join(strings.Fields(text), " ") {
		t.Errorf("lost words: %q", joined)
	}
	if got := splitScript("  \n ", 50); len(got) != 0 {
		t.Errorf("blank: %q", got)
	}
}

func TestJoinWAV(t *testing.T) {
	joined, format, pcm, err := joinWAV([][]byte{testWAV(1, 2), testWAV(3)})
	if err != nil {
		t.Fatal(err)
	}
	pause := int(speechPartPause.Seconds()*48000) &^ 1
	if format.Rate != 24000 || len(pcm) != 6+pause {
		t.Fatalf("%+v, %d bytes", format, len(pcm))
	}
	f, samples, err := parseWAV(joined)
	if err != nil || f != format || len(samples) != len(pcm) {
		t.Fatalf("not a file: %v %+v", err, f)
	}
	if int16(binary.LittleEndian.Uint16(samples[len(samples)-2:])) != 3 {
		t.Fatal("the parts are out of order")
	}
	other := testWAV(1)
	binary.LittleEndian.PutUint32(other[24:], 16000)
	if _, _, _, err := joinWAV([][]byte{testWAV(1), other}); err == nil {
		t.Error("joined two formats")
	}
	if _, _, _, err := joinWAV([][]byte{[]byte("nope")}); err == nil {
		t.Error("joined something that is not a WAV")
	}
	// A chunk before the samples is skipped.
	extra := testWAV(7)
	list := append(append(append([]byte{}, extra[:36]...), []byte("LIST\x02\x00\x00\x00ab")...), extra[36:]...)
	if _, s, err := parseWAV(list); err != nil || len(s) != 2 {
		t.Errorf("extra chunk: %v %d", err, len(s))
	}
}

// A part's own silence goes, so every gap is the same; the outer edges stay, and an
// edge the model cut mid-sound fades instead of clicking.
func TestShapeEdges(t *testing.T) {
	format := wavFormat{Channels: 1, Bits: 16, Rate: 24000}
	silence := make([]int16, 24000) // a second
	tone := make([]int16, 2400)
	for i := range tone {
		tone[i] = 10000
	}
	make16 := func(parts ...[]int16) []byte {
		var all []int16
		for _, p := range parts {
			all = append(all, p...)
		}
		_, pcm, _ := parseWAV(testWAV(all...))
		return pcm
	}
	keep := int(speechEdgeKeep.Seconds()*24000) * 2
	got := shapeEdges(make16(silence, tone, silence), format, true, true)
	if len(got) != len(tone)*2+2*keep {
		t.Errorf("trimmed to %d bytes, want %d", len(got), len(tone)*2+2*keep)
	}
	// Not asked to trim that side: it stays.
	if got := shapeEdges(make16(silence, tone), format, false, false); len(got) != (len(silence)+len(tone))*2 {
		t.Errorf("an outer edge changed: %d", len(got))
	}
	// Cut mid-sound: the last sample is nearly silent, and the middle is untouched.
	cut := shapeEdges(make16(tone), format, false, true)
	last := int16(binary.LittleEndian.Uint16(cut[len(cut)-2:]))
	if last > 100 || last < -100 || int16(binary.LittleEndian.Uint16(cut[100:])) != 10000 {
		t.Errorf("fade: last %d", last)
	}
	// The input is not changed, and nothing of a silent part breaks.
	original := make16(tone)
	shapeEdges(original, format, false, true)
	if int16(binary.LittleEndian.Uint16(original[len(original)-2:])) != 10000 {
		t.Error("changed the input")
	}
	if got := shapeEdges(make16(silence), format, true, true); len(got) > 2*keep+2 {
		t.Errorf("a silent part: %d bytes", len(got))
	}
}

func TestWavePeaks(t *testing.T) {
	samples := make([]int16, speechPeaks*10)
	samples[0], samples[len(samples)-1] = 10000, -20000
	_, _, pcm, _ := joinWAV([][]byte{testWAV(samples...)})
	bars := wavePeaks(pcm)
	if len(bars) != speechPeaks || bars[speechPeaks-1].H != speechBarMax || bars[0].H != speechBarMax/2 || bars[5].H != 2 {
		t.Fatalf("bars: %+v", bars)
	}
	if wavePeaks(nil) != nil {
		t.Error("bars for nothing")
	}
}

// A script runs part by part: progress, a joined file, the cut flag, Stop and
// a failure part way through.
func TestSpeechJob(t *testing.T) {
	var mu sync.Mutex
	var inputs []string
	gate := make(chan struct{}, 100)
	fail := ""
	cut := false
	u := New(Deps{NoAuth: true, Call: func(ctx context.Context, _ *auth.Principal, _, path, _ string, body []byte) APIResult {
		if path != "/v1/audio/speech" {
			return APIResult{Status: 200, Body: []byte(`{"data":[]}`)}
		}
		var req struct{ Input string }
		json.Unmarshal(body, &req)
		select {
		case <-gate:
		case <-ctx.Done():
			return APIResult{Status: 499}
		}
		mu.Lock()
		defer mu.Unlock()
		inputs = append(inputs, req.Input)
		if fail != "" && strings.Contains(req.Input, fail) {
			return APIResult{Status: 500, Body: []byte(`{"error":{"message":"It broke."}}`)}
		}
		h := http.Header{}
		if cut {
			h.Set("X-Localbase-Audio-Truncated", "true")
		}
		return APIResult{Status: 200, Header: h, Body: testWAV(1000, -2000)}
	}})
	script := strings.Repeat("A sentence that takes some room to say out loud, really. ", 10)
	start := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		u.ServeHTTP(w, htmxPost("/app/lab/speech", url.Values{"model": {"m"}, "input": {script}, "voice": {"default"}}))
		return w
	}

	// Runs part by part and says so; a second script waits.
	w := start()
	first := w.Body.String()
	if !strings.Contains(first, "Making part 1 of") || !strings.Contains(first, "<progress") || !strings.Contains(first, "it keeps going") {
		t.Fatalf("progress: %s", first)
	}
	w2 := httptest.NewRecorder()
	u.ServeHTTP(w2, htmxPost("/app/lab/speech", url.Values{"model": {"m"}, "input": {"hi"}, "voice": {"default"}}))
	if !strings.Contains(w2.Body.String(), "One script at a time") {
		t.Fatalf("second: %s", w2.Body.String())
	}
	for range 20 {
		gate <- struct{}{}
	}
	done := waitSpeech(t, u, w)
	if !strings.Contains(done, "/app/lab/results/") || !strings.Contains(done, "<rect") || !strings.Contains(done, "Make again") {
		t.Fatalf("done: %s", done)
	}
	mu.Lock()
	parts := len(inputs)
	for _, in := range inputs {
		if utf8.RuneCountInString(in) > speechPartLimit {
			t.Errorf("a part over the limit: %q", in)
		}
	}
	if strings.Join(strings.Fields(strings.Join(inputs, " ")), " ") != strings.TrimSpace(script) {
		t.Error("the parts are not the script")
	}
	inputs, cut = nil, true
	mu.Unlock()
	if parts < 2 {
		t.Fatalf("%d parts", parts)
	}

	// The API's cut flag is said in words.
	w = start()
	for range 20 {
		gate <- struct{}{}
	}
	if html := waitSpeech(t, u, w); !strings.Contains(html, "cut part of this short") {
		t.Errorf("cut: %s", html)
	}
	mu.Lock()
	cut, inputs = false, nil
	mu.Unlock()
	for len(gate) > 0 {
		<-gate
	}

	// Stop: what was made stays.
	w = start()
	gate <- struct{}{}
	m := runIDRe.FindStringSubmatch(w.Body.String())
	waitPart(t, u, m[1])
	r := htmxPost("/app/lab/speech/"+m[1]+"/stop", url.Values{})
	withCSRF(&UI{}, r)
	u.ServeHTTP(httptest.NewRecorder(), r)
	if html := waitSpeech(t, u, w); !strings.Contains(html, "Stopped after part 1 of") || !strings.Contains(html, "/app/lab/results/") {
		t.Errorf("stop: %s", html)
	}

	// A failure part way keeps what came before, and says where.
	mu.Lock()
	inputs, fail = nil, "really"
	mu.Unlock()
	w = start()
	gate <- struct{}{}
	if html := waitSpeech(t, u, w); !strings.Contains(html, "It broke.") || !strings.Contains(html, `role="alert"`) || !strings.Contains(html, "Try again") {
		t.Errorf("fail: %s", html)
	}

	// Past the limit nothing is cut silently.
	w = httptest.NewRecorder()
	u.ServeHTTP(w, htmxPost("/app/lab/speech", url.Values{"model": {"m"}, "input": {strings.Repeat("x", speechScriptLimit+1)}, "voice": {"default"}}))
	if !strings.Contains(w.Body.String(), "Nothing was cut") {
		t.Errorf("limit: %s", w.Body.String())
	}
}

var runIDRe = regexp.MustCompile(`id="run-(sp[0-9a-f]+)"`)

func time0() { time.Sleep(2 * time.Millisecond) }

// waitPart waits for a script's first part to be counted.
func waitPart(t *testing.T, u *UI, id string) {
	t.Helper()
	for i := 0; i < 500; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/app/lab/speech/"+id, nil)
		r.Header.Set("HX-Request", "true")
		u.ServeHTTP(w, r)
		if strings.Contains(w.Body.String(), "Making part 2 of") {
			return
		}
		time0()
	}
	t.Fatal("the first part never finished")
}
