package contract

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestDifferentialVideoRejections sends video requests that never start a
// job to both gateways (the harness selects wan2.1-t2v-1.3b-q8_0 and
// fastwan2.2-ti2v-5b-q6_k without their files): validation, profile
// mismatches, unknown models, install consent, the multipart form, and the
// job routes for jobs that do not exist. Status, error type, code, param,
// message and Allow must match.
func TestDifferentialVideoRejections(t *testing.T) {
	send := func(method, path, ctype, body string) map[string]*Resp {
		out := map[string]*Resp{}
		for _, tg := range targets {
			h := bearer(tg.Key)
			if ctype != "" {
				h["Content-Type"] = ctype
			}
			out[tg.Name] = do(t, method, tg.BaseURL+path, h, body)
		}
		return out
	}
	request := func(model string, w, h, frames, fps int, more map[string]any) string {
		body := map[string]any{"model": model, "prompt": "a lighthouse at dusk", "width": w, "height": h,
			"frames": frames, "fps": fps, "input": map[string]any{"kind": "text"}}
		for k, v := range more {
			if v == nil {
				delete(body, k)
			} else {
				body[k] = v
			}
		}
		b, _ := json.Marshal(body)
		return string(b)
	}
	const wan, fastwan = "wan2.1-t2v-1.3b-q8_0", "fastwan2.2-ti2v-5b-q6_k"
	ref := base64.StdEncoding.EncodeToString(testPNG(480, 832))
	small := base64.StdEncoding.EncodeToString(testPNG(16, 24))
	cases := map[string]string{
		"array":             `[]`,
		"string":            `"video"`,
		"invalid json":      `{"model":`,
		"empty":             `{}`,
		"empty model":       request("", 320, 320, 33, 16, nil),
		"long prompt":       request(wan, 320, 320, 33, 16, map[string]any{"prompt": strings.Repeat("x", 16_385)}),
		"string width":      request(wan, 320, 320, 33, 16, map[string]any{"width": "320"}),
		"fractional height": request(wan, 320, 320, 33, 16, map[string]any{"height": 320.5}),
		"zero frames":       request(wan, 320, 320, 0, 16, nil),
		"extra key":         request(wan, 320, 320, 33, 16, map[string]any{"seconds": 4}),
		"no input":          request(wan, 320, 320, 33, 16, map[string]any{"input": nil}),
		"unknown input":     request(wan, 320, 320, 33, 16, map[string]any{"input": map[string]any{"kind": "audio"}}),
		"input extra":       request(wan, 320, 320, 33, 16, map[string]any{"input": map[string]any{"kind": "text", "x": 1}}),
		"image format": request(wan, 320, 320, 33, 16, map[string]any{"input": map[string]any{"kind": "image",
			"image": map[string]any{"format": "jpeg", "data": "abcd"}}}),
		"reference with image": request(fastwan, 480, 832, 81, 16, map[string]any{"input_reference": ref,
			"input": map[string]any{"kind": "image", "image": map[string]any{"format": "png", "data": ref}}}),
		"unknown model":        request("no-such-video", 320, 320, 33, 16, nil),
		"chat model":           request(chatModel, 320, 320, 33, 16, nil),
		"wrong size":           request(wan, 336, 320, 33, 16, nil),
		"wrong frames":         request(wan, 320, 320, 32, 16, nil),
		"wrong fps":            request(wan, 320, 320, 33, 24, nil),
		"t2v with reference":   request(wan, 320, 320, 33, 16, map[string]any{"input_reference": ref}),
		"ti2v wrong size":      request(fastwan, 496, 832, 81, 16, nil),
		"ti2v small reference": request(fastwan, 480, 832, 81, 16, map[string]any{"input_reference": small}),
		"ti2v data url reference": request(fastwan, 480, 832, 81, 16, map[string]any{
			"input_reference": "data:image/png;base64," + small}),
		"ti2v files missing": request(fastwan, 480, 832, 81, 16, map[string]any{"input_reference": ref}),
	}
	for label, body := range cases {
		sameError(t, "create "+label, send("POST", "/v1/videos", "application/json", body))
	}
	sameError(t, "create GET", send("GET", "/v1/videos", "", ""))

	form := func(label string, parts ...imgPart) {
		body, ct := imageForm(parts...)
		sameError(t, "multipart "+label, send("POST", "/v1/videos", ct, body))
	}
	fields := []imgPart{field("model", fastwan), field("prompt", "a fox"), field("width", "480"), field("height", "832"),
		field("frames", "81"), field("fps", "16")}
	form("files missing", fields...)
	form("no width", append([]imgPart{}, fields[0], fields[1], fields[3], fields[4], fields[5])...)
	form("width text", append(append([]imgPart{}, fields[:2]...), field("width", "wide"), fields[3], fields[4], fields[5])...)
	form("input json", append(append([]imgPart{}, fields...), field("input", "{kind"))...)
	form("input image", append(append([]imgPart{}, fields...), field("input", `{"kind":"image"}`))...)
	jpg := file("input_reference", "image/jpeg", testJPEG(8, 8))
	form("jpeg reference", append(append([]imgPart{}, fields...), jpg)...)
	pngRef := file("input_reference", "image/png", testPNG(480, 832))
	form("ti2v png reference", append(append([]imgPart{}, fields...), pngRef)...)
	form("t2v reference", append([]imgPart{field("model", wan), field("prompt", "a fox"), field("width", "320"),
		field("height", "320"), field("frames", "33"), field("fps", "16")}, pngRef)...)
	sameError(t, "multipart no boundary", send("POST", "/v1/videos", "multipart/form-data; charset=utf-8", "x"))

	const id = "6f1c2c3e-8a7b-4c1d-9e2f-0123456789ab"
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/videos/" + id},
		{"DELETE", "/v1/videos/" + id},
		{"PUT", "/v1/videos/" + id},
		{"GET", "/v1/videos/" + id + "/content"},
		{"POST", "/v1/videos/" + id + "/content"},
		{"POST", "/v1/videos/" + id + "/cancel"},
		{"GET", "/v1/videos/" + id + "/cancel"},
		{"GET", "/v1/videos/not-a-job"},
		{"GET", "/v1/videos/" + id + "/frames"},
	} {
		sameError(t, c.method+" "+c.path, send(c.method, c.path, "", ""))
	}
}

// TestDifferentialVideoJobFailure starts the same job on both gateways. The
// harness's wan2.1 files are sparse placeholders at the catalog sizes, so
// both accept the job (202) and both must end it failed with the same
// public error once the runtime refuses the weights.
func TestDifferentialVideoJobFailure(t *testing.T) {
	body := `{"model":"wan2.1-t2v-1.3b-q8_0","prompt":"a lighthouse at dusk","width":320,"height":320,"frames":33,"fps":16,"input":{"kind":"text"}}`
	final := map[string]map[string]any{}
	for _, tg := range targets {
		h := bearer(tg.Key)
		h["Content-Type"] = "application/json"
		r := do(t, "POST", tg.BaseURL+"/v1/videos", h, body)
		if r.Status != 202 {
			t.Fatalf("%s: %d %s", tg.Name, r.Status, r.Body)
		}
		job := r.JSON(t)
		if job["status"] != "queued" || job["object"] != "localbase.video.job" {
			t.Fatalf("%s: %v", tg.Name, job)
		}
		id := job["id"].(string)
		deadline := time.Now().Add(5 * time.Minute)
		for {
			s := do(t, "GET", tg.BaseURL+"/v1/videos/"+id, bearer(tg.Key), "").JSON(t)
			if s["status"] != "queued" && s["status"] != "in_progress" {
				final[tg.Name] = s
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: job still %v", tg.Name, s["status"])
			}
			time.Sleep(250 * time.Millisecond)
		}
		if c := do(t, "GET", tg.BaseURL+"/v1/videos/"+id+"/content", bearer(tg.Key), ""); c.Status != 404 {
			t.Errorf("%s: content of a failed job answered %d", tg.Name, c.Status)
		}
	}
	var first map[string]any
	for name, s := range final {
		t.Logf("%s: %v", name, s["error"])
		if s["status"] != "failed" {
			t.Errorf("%s: %v", name, s)
		}
		if first == nil {
			first = s
			continue
		}
		if fmt.Sprint(s["error"]) != fmt.Sprint(first["error"]) {
			t.Errorf("errors differ: %v vs %v", final["go"]["error"], final["ts"]["error"])
		}
	}
}
