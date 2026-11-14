package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"mime/multipart"
	"net/textproto"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness"
)

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

// wantError requires every target to answer label with an error: a status of 400 or more and a
// whole error envelope. (Until step 7 it also required TS and Go to answer alike.)
func wantError(t *testing.T, label string, rs map[string]*harness.Resp) {
	t.Helper()
	for _, tg := range harness.Targets(t) {
		r := rs[tg.Name]
		if r.Status < 400 {
			t.Errorf("%s: %s answered %d %s", label, tg.Name, r.Status, r.Body)
			continue
		}
		r.ErrorCode(t)
	}
}

// TestDifferentialVoices runs the custom voice lifecycle: an upload stores a normalized recording,
// and every rejection answers with an error envelope. (Until step 7 TS and Go had to agree.)
func TestDifferentialVoices(t *testing.T) {
	const base = "/_localbase/voices"
	type call struct {
		method, path, key, ctype, body string
	}
	run := func(c func(tg *harness.Target) call) map[string]*harness.Resp {
		out := map[string]*harness.Resp{}
		for _, tg := range harness.Targets(t) {
			x := c(tg)
			h := map[string]string{}
			if x.key != "" {
				h = harness.Bearer(x.key)
			}
			if x.ctype != "" {
				h["Content-Type"] = x.ctype
			}
			out[tg.Name] = harness.Do(t, x.method, tg.BaseURL+x.path, h, x.body)
		}
		return out
	}
	admin := func(method, path, ctype, body string) map[string]*harness.Resp {
		return run(func(tg *harness.Target) call { return call{method, path, tg.Admin, ctype, body} })
	}
	create := func(parts ...voicePart) map[string]*harness.Resp {
		body, ct := voiceForm(parts...)
		return admin("POST", base, ct, body)
	}
	clip := harness.VoiceClip(7)

	// The list, as a reader: limits and the consent statement, no voices.
	list := run(func(tg *harness.Target) call { return call{"GET", base, tg.Key, "", ""} })
	var firstList string
	for _, tg := range harness.Targets(t) {
		r := list[tg.Name]
		if r.Status != 200 || r.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s list: %d %s", tg.Name, r.Status, r.Body)
		}
		if firstList == "" {
			firstList = string(r.Body)
		} else if d := harness.DiffJSON("", mustJSON(t, firstList), r.JSON(t), nil); len(d) > 0 {
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
		wantError(t, "create "+c.label, create(c.parts...))
	}
	wantError(t, "create as json", admin("POST", base, "application/json", `{"name":"Narrator"}`))
	// A form without a boundary is 400 validation_failed (TS answered 500 gateway_error).
	for name, r := range admin("POST", base, "multipart/form-data; charset=utf-8", "x") {
		if r.Status != 400 {
			t.Errorf("%s create without a boundary: %d %s", name, r.Status, r.Body)
		}
		r.ErrorCode(t)
	}
	wantError(t, "PUT collection", admin("PUT", base, "", ""))
	reader := func(method, path, ctype, body string) map[string]*harness.Resp {
		return run(func(tg *harness.Target) call { return call{method, path, tg.Key, ctype, body} })
	}
	body, ct := voiceForm(voicePart{name: "name", value: "Narrator"}, voicePart{name: "consent", value: "true"}, voicePart{name: "audio", file: clip})
	wantError(t, "create as reader", reader("POST", base, ct, body))
	wantError(t, "list as chat-only", run(func(tg *harness.Target) call { return call{"GET", base, tg.ChatOnly, "", ""} }))
	wantError(t, "list without credentials", run(func(tg *harness.Target) call { return call{"GET", base, "", "", ""} }))
	wantError(t, "unknown voice", admin("GET", base+"/voice-000000000000", "", ""))
	wantError(t, "unknown voice audio", admin("GET", base+"/voice-000000000000/audio", "", ""))
	wantError(t, "delete unknown voice", admin("DELETE", base+"/voice-000000000000", "", ""))
	wantError(t, "malformed id", admin("GET", base+"/voice-XYZ", "", ""))
	wantError(t, "extra segment", admin("GET", base+"/voice-000000000000/audio/x", "", ""))

	// Create: a recording.
	created := create(voicePart{name: "name", value: "  Narrator\t Voice "}, voicePart{name: "consent", value: "true"}, voicePart{name: "audio", file: clip})
	ids := map[string]string{}
	var want map[string]any
	for _, tg := range harness.Targets(t) {
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
		if d := harness.DiffJSON("", want, v, func(p string) bool {
			return p == ".id" || p == ".createdAt" || p == ".consent.attestedAt" || p == ".createdBy"
		}); len(d) > 0 {
			t.Errorf("create differs: %v", d)
		}
	}
	if want["name"] != "Narrator Voice" {
		t.Errorf("name %v", want["name"])
	}
	at := func(suffix string) func(tg *harness.Target) string {
		return func(tg *harness.Target) string { return base + "/" + ids[tg.Name] + suffix }
	}
	on := func(method string, path func(*harness.Target) string, key func(*harness.Target) string, ctype, body string) map[string]*harness.Resp {
		return run(func(tg *harness.Target) call { return call{method, path(tg), key(tg), ctype, body} })
	}
	asAdmin := func(tg *harness.Target) string { return tg.Admin }
	asReader := func(tg *harness.Target) string { return tg.Key }

	// Audio: managers only; the stored bytes match the recorded checksum.
	for name, r := range on("GET", at("/audio"), asAdmin, "", "") {
		sum := sha256.Sum256(r.Body)
		if r.Status != 200 || r.Header.Get("Content-Type") != "audio/wav" || hex.EncodeToString(sum[:]) != want["sha256"] ||
			r.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s audio: %d %s", name, r.Status, r.Header)
		}
	}
	wantError(t, "audio as reader", on("GET", at("/audio"), asReader, "", ""))
	wantError(t, "POST audio", on("POST", at("/audio"), asAdmin, "", ""))
	wantError(t, "POST voice", on("POST", at(""), asAdmin, "", ""))
	wantError(t, "duplicate name", create(voicePart{name: "name", value: "narrator voice"}, voicePart{name: "consent", value: "true"}, voicePart{name: "audio", file: clip}))

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
		wantError(t, "rename "+label, on("PATCH", at(""), asAdmin, "application/json", b))
	}
	wantError(t, "rename as reader", on("PATCH", at(""), asReader, "application/json", `{"name":"Other"}`))
	wantError(t, "rename unknown", admin("PATCH", base+"/voice-000000000000", "application/json", `{"name":"Other"}`))
	for name, r := range on("PATCH", at(""), asAdmin, "application/json", `{"name":"Storyteller"}`) {
		if v := r.JSON(t); r.Status != 200 || v["name"] != "Storyteller" || v["sha256"] != want["sha256"] {
			t.Errorf("%s rename: %d %s", name, r.Status, r.Body)
		}
	}

	// The voice is usable on the speech route's checks: a deleted one is not.
	wantError(t, "delete as reader", on("DELETE", at(""), asReader, "", ""))
	for name, r := range on("DELETE", at(""), asAdmin, "", "") {
		if v := r.JSON(t); r.Status != 200 || v["deleted"] != true || v["object"] != "localbase.voice" || v["id"] == nil {
			t.Errorf("%s delete: %d %s", name, r.Status, r.Body)
		}
	}
	wantError(t, "get deleted", on("GET", at(""), asAdmin, "", ""))
	wantError(t, "speech with a deleted voice", run(func(tg *harness.Target) call {
		return call{"POST", "/v1/audio/speech", tg.Key, "application/json",
			`{"model":"qwen3-tts-1.7b-base-q4_k_m","input":"hi","voice":"` + ids[tg.Name] + `"}`}
	}))
}

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	return (&harness.Resp{Body: []byte(s)}).JSON(t)
}
