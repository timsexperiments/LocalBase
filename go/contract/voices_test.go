package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// voiceClip is a 4 s 16 kHz PCM16 WAV of deterministic noise.
func voiceClip(seed uint32) []byte {
	const frames = 64000
	b := make([]byte, 44, 44+frames*2)
	copy(b, "RIFF")
	copy(b[8:], "WAVEfmt ")
	le := binary.LittleEndian
	le.PutUint32(b[16:], 16)
	le.PutUint16(b[20:], 1)
	le.PutUint16(b[22:], 1)
	le.PutUint32(b[24:], 16000)
	le.PutUint32(b[28:], 32000)
	le.PutUint16(b[32:], 2)
	le.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	le.PutUint32(b[40:], frames*2)
	x := seed
	for i := 0; i < frames*2; i++ {
		x = x*1664525 + 1013904223
		b = append(b, byte(x>>24))
	}
	le.PutUint32(b[4:], uint32(len(b)-8))
	return b
}

type voicePart struct {
	name, value string
	file        []byte // a file part when non-nil
}

func voiceForm(parts ...voicePart) (string, string) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for _, p := range parts {
		if p.file != nil {
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", `form-data; name="`+p.name+`"; filename="clip.wav"`)
			h.Set("Content-Type", "audio/wav")
			fw, _ := w.CreatePart(h)
			fw.Write(p.file)
			continue
		}
		w.WriteField(p.name, p.value)
	}
	w.Close()
	return b.String(), w.FormDataContentType()
}

// sameError requires the same status and error envelope on every target.
func sameError(t *testing.T, label string, rs map[string]*Resp) {
	t.Helper()
	var first *Resp
	var firstName string
	for _, tg := range targets {
		r := rs[tg.Name]
		if r.Status < 400 {
			t.Errorf("%s: %s answered %d %s", label, tg.Name, r.Status, r.Body)
			return
		}
		if first == nil {
			first, firstName = r, tg.Name
			continue
		}
		ea, _ := first.JSON(t)["error"].(map[string]any)
		eb, _ := r.JSON(t)["error"].(map[string]any)
		for _, k := range []string{"type", "code", "param", "message"} {
			if ea[k] != eb[k] || first.Status != r.Status {
				t.Errorf("%s:\n %s %d %s\n %s %d %s", label, firstName, first.Status, first.Body, tg.Name, r.Status, r.Body)
				return
			}
		}
		if a, b := first.Header.Get("Allow"), r.Header.Get("Allow"); a != b {
			t.Errorf("%s: Allow %q vs %q", label, a, b)
		}
	}
}

// TestDifferentialVoices runs the custom voice lifecycle on both gateways:
// the same upload must store the same normalized recording, and every
// rejection must match.
func TestDifferentialVoices(t *testing.T) {
	const base = "/_localbase/voices"
	type call struct {
		method, path, key, ctype, body string
	}
	run := func(c func(tg *Target) call) map[string]*Resp {
		out := map[string]*Resp{}
		for _, tg := range targets {
			x := c(tg)
			h := map[string]string{}
			if x.key != "" {
				h = bearer(x.key)
			}
			if x.ctype != "" {
				h["Content-Type"] = x.ctype
			}
			out[tg.Name] = do(t, x.method, tg.BaseURL+x.path, h, x.body)
		}
		return out
	}
	admin := func(method, path, ctype, body string) map[string]*Resp {
		return run(func(tg *Target) call { return call{method, path, tg.Admin, ctype, body} })
	}
	create := func(parts ...voicePart) map[string]*Resp {
		body, ct := voiceForm(parts...)
		return admin("POST", base, ct, body)
	}
	clip := voiceClip(7)

	// The list, as a reader: limits and the consent statement, no voices.
	list := run(func(tg *Target) call { return call{"GET", base, tg.Key, "", ""} })
	var firstList string
	for _, tg := range targets {
		r := list[tg.Name]
		if r.Status != 200 || r.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s list: %d %s", tg.Name, r.Status, r.Body)
		}
		if firstList == "" {
			firstList = string(r.Body)
		} else if d := diffJSON("", mustJSON(t, firstList), r.JSON(t), nil); len(d) > 0 {
			t.Errorf("list differs: %v", d)
		}
	}

	// Rejections before anything is stored.
	rejections := []struct {
		label string
		parts []voicePart
	}{
		{"no consent", []voicePart{{name: "name", value: "Narrator"}, {name: "audio", file: clip}}},
		{"consent not true", []voicePart{{name: "name", value: "Narrator"}, {name: "consent", value: "yes"}, {name: "audio", file: clip}}},
		{"reserved name", []voicePart{{name: "name", value: "ＤＥＦＡＵＬＴ"}, {name: "consent", value: "true"}, {name: "audio", file: clip}}},
		{"id-shaped name", []voicePart{{name: "name", value: "voice-0123456789ab"}, {name: "consent", value: "true"}, {name: "audio", file: clip}}},
		{"long name", []voicePart{{name: "name", value: strings.Repeat("n", 49)}, {name: "consent", value: "true"}, {name: "audio", file: clip}}},
		{"missing name", []voicePart{{name: "consent", value: "true"}, {name: "audio", file: clip}}},
		{"name as file", []voicePart{{name: "name", file: []byte("x")}, {name: "consent", value: "true"}, {name: "audio", file: clip}}},
		{"audio as text", []voicePart{{name: "name", value: "Narrator"}, {name: "consent", value: "true"}, {name: "audio", value: "RIFF"}}},
		{"not wav", []voicePart{{name: "name", value: "Narrator"}, {name: "consent", value: "true"}, {name: "audio", file: []byte("ID3 not a wav")}}},
		{"too short", []voicePart{{name: "name", value: "Narrator"}, {name: "consent", value: "true"}, {name: "audio", file: clip[:44+16000*2*2]}}},
	}
	for _, c := range rejections {
		sameError(t, "create "+c.label, create(c.parts...))
	}
	sameError(t, "create as json", admin("POST", base, "application/json", `{"name":"Narrator"}`))
	// Deliberate difference: TS fails to parse the form and answers 500
	// gateway_error; Go answers 400 validation_failed. Both use the envelope.
	for name, r := range admin("POST", base, "multipart/form-data; charset=utf-8", "x") {
		if want := map[string]int{"ts": 500, "go": 400}[name]; r.Status != want {
			t.Errorf("%s create without a boundary: %d %s", name, r.Status, r.Body)
		}
		r.ErrorCode(t)
	}
	sameError(t, "PUT collection", admin("PUT", base, "", ""))
	reader := func(method, path, ctype, body string) map[string]*Resp {
		return run(func(tg *Target) call { return call{method, path, tg.Key, ctype, body} })
	}
	body, ct := voiceForm(voicePart{name: "name", value: "Narrator"}, voicePart{name: "consent", value: "true"}, voicePart{name: "audio", file: clip})
	sameError(t, "create as reader", reader("POST", base, ct, body))
	sameError(t, "list as chat-only", run(func(tg *Target) call { return call{"GET", base, tg.ChatOnly, "", ""} }))
	sameError(t, "list without credentials", run(func(tg *Target) call { return call{"GET", base, "", "", ""} }))
	sameError(t, "unknown voice", admin("GET", base+"/voice-000000000000", "", ""))
	sameError(t, "unknown voice audio", admin("GET", base+"/voice-000000000000/audio", "", ""))
	sameError(t, "delete unknown voice", admin("DELETE", base+"/voice-000000000000", "", ""))
	sameError(t, "malformed id", admin("GET", base+"/voice-XYZ", "", ""))
	sameError(t, "extra segment", admin("GET", base+"/voice-000000000000/audio/x", "", ""))

	// Create: the same recording on both.
	created := create(voicePart{name: "name", value: "  Narrator\t Voice "}, voicePart{name: "consent", value: "true"}, voicePart{name: "audio", file: clip})
	ids := map[string]string{}
	var want map[string]any
	for _, tg := range targets {
		r := created[tg.Name]
		if r.Status != 201 {
			t.Fatalf("%s create: %d %s", tg.Name, r.Status, r.Body)
		}
		v := r.JSON(t)
		ids[tg.Name] = v["id"].(string)
		if !strings.HasPrefix(v["createdBy"].(string), "api-key:") {
			t.Errorf("%s createdBy %v", tg.Name, v["createdBy"])
		}
		if want == nil {
			want = v
			continue
		}
		if d := diffJSON("", want, v, func(p string) bool {
			return p == ".id" || p == ".createdAt" || p == ".consent.attestedAt" || p == ".createdBy"
		}); len(d) > 0 {
			t.Errorf("create differs: %v", d)
		}
	}
	if want["name"] != "Narrator Voice" {
		t.Errorf("name %v", want["name"])
	}
	at := func(suffix string) func(tg *Target) string {
		return func(tg *Target) string { return base + "/" + ids[tg.Name] + suffix }
	}
	on := func(method string, path func(*Target) string, key func(*Target) string, ctype, body string) map[string]*Resp {
		return run(func(tg *Target) call { return call{method, path(tg), key(tg), ctype, body} })
	}
	asAdmin := func(tg *Target) string { return tg.Admin }
	asReader := func(tg *Target) string { return tg.Key }

	// Audio: managers only; the stored bytes match the recorded checksum.
	for name, r := range on("GET", at("/audio"), asAdmin, "", "") {
		sum := sha256.Sum256(r.Body)
		if r.Status != 200 || r.Header.Get("Content-Type") != "audio/wav" || hex.EncodeToString(sum[:]) != want["sha256"] ||
			r.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s audio: %d %s", name, r.Status, r.Header)
		}
	}
	sameError(t, "audio as reader", on("GET", at("/audio"), asReader, "", ""))
	sameError(t, "POST audio", on("POST", at("/audio"), asAdmin, "", ""))
	sameError(t, "POST voice", on("POST", at(""), asAdmin, "", ""))
	sameError(t, "duplicate name", create(voicePart{name: "name", value: "narrator voice"}, voicePart{name: "consent", value: "true"}, voicePart{name: "audio", file: clip}))

	// Readers see the voice without its creator.
	for name, r := range on("GET", at(""), asReader, "", "") {
		if v := r.JSON(t); r.Status != 200 || v["createdBy"] != nil || v["name"] != "Narrator Voice" {
			t.Errorf("%s get as reader: %d %s", name, r.Status, r.Body)
		}
	}

	// Rename.
	for label, b := range map[string]string{
		"not json": `{`, "array": `[]`, "null": `null`, "missing name": `{}`, "number name": `{"name":5}`,
		"extra keys": `{"name":"Ok","b":1,"a":2}`, "extra key": `{"name":"Ok","z":1}`, "reserved": `{"name":"Willow"}`,
		"blank": `{"name":"   "}`, "too large": `{"name":"` + strings.Repeat("x", 1100) + `"}`,
	} {
		sameError(t, "rename "+label, on("PATCH", at(""), asAdmin, "application/json", b))
	}
	sameError(t, "rename as reader", on("PATCH", at(""), asReader, "application/json", `{"name":"Other"}`))
	sameError(t, "rename unknown", admin("PATCH", base+"/voice-000000000000", "application/json", `{"name":"Other"}`))
	for name, r := range on("PATCH", at(""), asAdmin, "application/json", `{"name":"Storyteller"}`) {
		if v := r.JSON(t); r.Status != 200 || v["name"] != "Storyteller" || v["sha256"] != want["sha256"] {
			t.Errorf("%s rename: %d %s", name, r.Status, r.Body)
		}
	}

	// The voice is usable on the speech route's checks: a deleted one is not.
	sameError(t, "delete as reader", on("DELETE", at(""), asReader, "", ""))
	for name, r := range on("DELETE", at(""), asAdmin, "", "") {
		if v := r.JSON(t); r.Status != 200 || v["deleted"] != true || v["object"] != "localbase.voice" || v["id"] == nil {
			t.Errorf("%s delete: %d %s", name, r.Status, r.Body)
		}
	}
	sameError(t, "get deleted", on("GET", at(""), asAdmin, "", ""))
	sameError(t, "speech with a deleted voice", run(func(tg *Target) call {
		return call{"POST", "/v1/audio/speech", tg.Key, "application/json",
			`{"model":"qwen3-tts-1.7b-base-q4_k_m","input":"hi","voice":"` + ids[tg.Name] + `"}`}
	}))
}

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	return (&Resp{Body: []byte(s)}).JSON(t)
}

// TestCLIVoices compares `voices list|add|remove` on twin roots.
func TestCLIVoices(t *testing.T) {
	requireTS(t)
	ts, gr := twinRoots(t)
	roots := [2]string{ts, gr}
	for i, impl := range []string{"ts", "go"} {
		if r := runCLI(t, impl, roots[i], "--json", "init"); r.code != 0 {
			t.Fatalf("%s init: %s %s", impl, r.stdout, r.stderr)
		}
	}
	dir := t.TempDir()
	clip := filepath.Join(dir, "clip.wav")
	os.WriteFile(clip, voiceClip(11), 0o600)
	short := filepath.Join(dir, "short.wav")
	os.WriteFile(short, voiceClip(11)[:44+16000*2], 0o600)
	volatile := func(p string) bool { return strings.HasSuffix(p, ".id") }
	for _, c := range []cliCase{
		{args: []string{"--json", "voices", "list"}},
		{args: []string{"--json", "voices", "add", clip, "--name", "Narrator"}},
		{args: []string{"--json", "voices", "add", filepath.Join(dir, "missing.wav"), "--name", "Narrator", "--consent"}},
		{args: []string{"--json", "voices", "add", dir, "--name", "Narrator", "--consent"}},
		{args: []string{"--json", "voices", "add", short, "--name", "Narrator", "--consent"}},
		{args: []string{"--json", "voices", "add", clip, "--name", "Harbor", "--consent"}},
		{args: []string{"--json", "voices", "add", clip, "--name", " Narrator ", "--consent"}, skip: volatile},
		{args: []string{"--json", "voices", "add", clip, "--name", "narrator", "--consent"}},
		{args: []string{"--json", "voices", "list"}, skip: volatile},
		{args: []string{"--json", "voices", "remove", "voice-000000000000"}},
		{args: []string{"--json", "voices", "remove", "../etc"}},
	} {
		compareCLI(t, roots, c)
	}
	// Human output and removal of each root's own voice.
	var lines [2]string
	for i, impl := range []string{"ts", "go"} {
		r := runCLI(t, impl, roots[i], "--json", "voices", "list")
		id := normalizeID(t, r.stdout)
		h := runCLI(t, impl, roots[i], "voices", "list")
		lines[i] = strings.ReplaceAll(volatileValue.ReplaceAllString(h.stdout, "<time>"), id, "<id>")
		if rm := runCLI(t, impl, roots[i], "--json", "voices", "remove", id); rm.code != 0 {
			t.Errorf("%s remove: %s %s", impl, rm.stdout, rm.stderr)
		}
		if _, err := os.Stat(filepath.Join(roots[i], "voices", id+".wav")); !os.IsNotExist(err) {
			t.Errorf("%s kept the recording: %v", impl, err)
		}
	}
	if lines[0] != lines[1] {
		t.Errorf("voices list output:\n ts: %q\n go: %q", lines[0], lines[1])
	}
	compareCLI(t, roots, cliCase{args: []string{"--json", "voices", "list"}})
}

func normalizeID(t *testing.T, stdout string) string {
	t.Helper()
	var env struct {
		Data struct {
			Voices []struct{ ID string } `json:"voices"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil || len(env.Data.Voices) != 1 {
		t.Fatalf("voices list: %v %s", err, stdout)
	}
	return env.Data.Voices[0].ID
}
