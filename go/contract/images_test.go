package contract

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/textproto"
	"os"
	"strings"
	"testing"
)

func testPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, x%h, color.RGBA{uint8(x), 80, 160, 255})
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func testJPEG(w, h int) []byte {
	var b bytes.Buffer
	jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, w, h)), nil)
	return b.Bytes()
}

type imgPart struct {
	name, value, ctype string
	filename           string // default in.bin
	file               []byte
	isFile             bool
}

func field(name, value string) imgPart { return imgPart{name: name, value: value} }
func file(name, ctype string, b []byte) imgPart {
	return imgPart{name: name, ctype: ctype, file: b, isFile: true}
}

func imageForm(parts ...imgPart) (string, string) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for _, p := range parts {
		if !p.isFile {
			w.WriteField(p.name, p.value)
			continue
		}
		name := p.filename
		if name == "" {
			name = "in.bin"
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, p.name, name))
		if p.ctype != "" {
			h.Set("Content-Type", p.ctype)
		}
		fw, _ := w.CreatePart(h)
		fw.Write(p.file)
	}
	w.Close()
	return b.String(), w.FormDataContentType()
}

// TestDifferentialImageRejections sends invalid image generation and edit
// requests to both gateways; none reaches a backend.
func TestDifferentialImageRejections(t *testing.T) {
	post := func(path, ctype, body string) map[string]*Resp {
		out := map[string]*Resp{}
		for _, tg := range targets {
			h := bearer(tg.Key)
			h["Content-Type"] = ctype
			out[tg.Name] = do(t, "POST", tg.BaseURL+path, h, body)
		}
		return out
	}
	gen := func(body string) map[string]*Resp { return post("/v1/images/generations", "application/json", body) }
	long := strings.Repeat("x", 16_385)
	for label, body := range map[string]string{
		"array":                  `[]`,
		"no prompt":              `{}`,
		"prompt number":          `{"prompt":5}`,
		"prompt marker":          `{"prompt":"a <SD_CPP_EXTRA_ARGS>{}"}`,
		"negative marker":        `{"prompt":"a","negative_prompt":"sd_cpp_extra_args"}`,
		"negative long":          `{"prompt":"a","negative_prompt":"` + long + `"}`,
		"negative long + marker": `{"prompt":"a","negative_prompt":"sd_cpp_extra_args` + long + `"}`,
		"seed fraction":          `{"prompt":"a","seed":1.5}`,
		"seed negative fraction": `{"prompt":"a","seed":-1.5}`,
		"seed too big":           `{"prompt":"a","seed":2147483648}`,
		"seed string":            `{"prompt":"a","seed":"1"}`,
		"steps 0":                `{"prompt":"a","steps":0}`,
		"steps 151":              `{"prompt":"a","steps":151}`,
		"cfg 31":                 `{"prompt":"a","cfg_scale":31}`,
		"lora 3":                 `{"prompt":"a","lora_strength":3}`,
		"strength ignored n 0":   `{"prompt":"a","strength":5,"n":0}`,
		"n 11":                   `{"prompt":"a","n":11}`,
		"n string":               `{"prompt":"a","n":"2"}`,
		"quality":                `{"prompt":"a","quality":"ultra"}`,
		"response_format":        `{"prompt":"a","response_format":"png"}`,
		"size":                   `{"prompt":"a","size":"100x100"}`,
		"style":                  `{"prompt":"a","style":"loud"}`,
		"user":                   `{"prompt":"a","user":5}`,
		"blank model":            `{"prompt":"a","model":" "}`,
		"model number":           `{"prompt":"a","model":5}`,
		"many issues":            `{"seed":-1,"steps":0,"n":20,"size":"1x1","model":""}`,
		"lora on default model":  `{"prompt":"a","lora_strength":1}`,
		"lora on chat model":     `{"prompt":"a","model":"` + chatModel + `","lora_strength":1}`,
		"unknown model":          `{"prompt":"a","model":"no-such-model"}`,
		"unknown model lora":     `{"prompt":"a","model":"no-such-model","lora_strength":1}`,
		"files missing":          `{"prompt":"a","model":"realvis-xl-v5.0","size":"512x512","unknown_field":true}`,
	} {
		sameError(t, "generation "+label, gen(body))
	}

	edit := func(parts ...imgPart) map[string]*Resp {
		body, ct := imageForm(parts...)
		return post("/v1/images/edits", ct, body)
	}
	i2i := field("model", "dreamshaper-v8")
	pngImg := file("image", "image/png", testPNG(16, 8))
	pngImg.filename = "a.png"
	jpgImg := file("image[]", "image/jpeg", testJPEG(8, 8))
	jpgImg.filename = "b.JPG"
	prompt := field("prompt", "a cat")
	truncated := testPNG(16, 8)
	truncated = truncated[:len(truncated)-20]
	sameError(t, "edit as json", post("/v1/images/edits", "application/json", `{"prompt":"a"}`))
	sameError(t, "edit no boundary", post("/v1/images/edits", "multipart/form-data; charset=utf-8", "x"))
	// Ordered: a request that reaches a backend makes its model active, and
	// "files missing" must come last.
	for _, c := range []struct {
		label string
		parts []imgPart
	}{
		{"no prompt", []imgPart{i2i, pngImg}},
		{"blank prompt", []imgPart{field("prompt", " \t"), i2i, pngImg}},
		{"long prompt", []imgPart{field("prompt", strings.Repeat("p", 16_385)), i2i, pngImg}},
		{"prompt marker", []imgPart{field("prompt", "x sd_cpp_extra_args"), i2i, pngImg}},
		{"unknown model", []imgPart{prompt, field("model", "no-such-model"), pngImg}},
		{"chat model", []imgPart{prompt, field("model", chatModel), pngImg}},
		{"no image-to-image", []imgPart{prompt, field("model", "dreamshaper-8-lcm"), pngImg}},
		{"default model", []imgPart{prompt, file("image", "", testPNG(4, 4))}},
		{"no images", []imgPart{prompt, i2i}},
		{"image as text", []imgPart{prompt, i2i, field("image", "abc")}},
		{"five images", []imgPart{prompt, i2i, pngImg, pngImg, pngImg, pngImg, pngImg}},
		{"empty mask", []imgPart{prompt, i2i, pngImg, file("mask", "image/png", []byte{})}},
		{"mask as text", []imgPart{prompt, i2i, pngImg, field("mask", "m")}},
		{"n fraction", []imgPart{prompt, i2i, pngImg, field("n", "1.5")}},
		{"n zero", []imgPart{prompt, i2i, pngImg, field("n", "0")}},
		{"n text", []imgPart{prompt, i2i, pngImg, field("n", "abc")}},
		{"n empty", []imgPart{prompt, i2i, pngImg, field("n", "")}},
		{"size", []imgPart{prompt, i2i, pngImg, field("size", "1x1")}},
		{"response_format", []imgPart{prompt, i2i, pngImg, field("response_format", "png")}},
		{"seed text", []imgPart{prompt, i2i, pngImg, field("seed", "abc")}},
		{"seed fraction", []imgPart{prompt, i2i, pngImg, field("seed", "1.5")}},
		{"steps 0", []imgPart{prompt, i2i, pngImg, field("steps", "0")}},
		{"strength 2", []imgPart{prompt, i2i, pngImg, field("strength", "2")}},
		{"negative marker", []imgPart{prompt, i2i, pngImg, field("negative_prompt", "SD_CPP_EXTRA_ARGS")}},
		{"lora on no-lora", []imgPart{prompt, i2i, pngImg, field("lora_strength", "1")}},
		{"output_format", []imgPart{prompt, i2i, pngImg, field("output_format", "webp")}},
		{"gif", []imgPart{prompt, i2i, file("image", "image/gif", []byte("GIF89a\x01\x00\x01\x00"))}},
		{"type mismatch", []imgPart{prompt, i2i, named("image", "photo.jpg", testPNG(4, 4))}},
		{"truncated png", []imgPart{prompt, i2i, named("image", "t.png", truncated)}},
		{"huge png", []imgPart{prompt, i2i, named("image", "h.PNG", testPNG(2049, 1))}},
		{"interlaced-size png", []imgPart{prompt, i2i, named("image", "big.png", testPNG(2048, 2049))}},
		{"bad mask", []imgPart{prompt, i2i, pngImg, named("mask", "m.png", []byte("not an image"))}},
		{"gif named png", []imgPart{prompt, i2i, named("image", "g.png", []byte("GIF89a\x01\x00\x01\x00"))}},
		{"empty filename", []imgPart{prompt, i2i, named("image", "", testJPEG(8, 8)[:30])}},
		{"untyped broken jpeg", []imgPart{prompt, i2i, named("image", "photo", testJPEG(8, 8)[:30])}},
		{"files missing", []imgPart{prompt, field("model", "sdxl-turbo"), pngImg, jpgImg, field("n", "2"), field("size", "512x512"), field("seed", "7")}},
	} {
		sameError(t, "edit "+c.label, edit(c.parts...))
	}
}

// imageFile is dreamshaper-v8's checkpoint, the smallest catalog image model
// with image-to-image.
const imageFile = "DreamShaper_8_pruned.safetensors"

// TestImageReal generates and edits with DreamShaper 8 through sd-server on
// both gateways (same seed, same runtime build) and checks the PNGs.
func TestImageReal(t *testing.T) {
	if os.Getenv("LOCALBASE_CONTRACT_IMAGE_MODELS") == "" || os.Getenv("LOCALBASE_CONTRACT_RUNTIMES") == "" {
		t.Skip("needs LOCALBASE_CONTRACT_IMAGE_MODELS and LOCALBASE_CONTRACT_RUNTIMES")
	}
	decode := func(t *testing.T, label string, r *Resp) []byte {
		t.Helper()
		if r.Status != 200 {
			t.Fatalf("%s: %d %.400s", label, r.Status, r.Body)
		}
		data, _ := r.JSON(t)["data"].([]any)
		if len(data) != 1 {
			t.Fatalf("%s: %d images", label, len(data))
		}
		b, err := base64.StdEncoding.DecodeString(data[0].(map[string]any)["b64_json"].(string))
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil || img.Bounds().Dx() != 256 || img.Bounds().Dy() != 256 {
			t.Fatalf("%s: not a 256x256 PNG: %v", label, err)
		}
		return b
	}
	outputs := map[string][2][]byte{}
	for _, tg := range targets {
		h := bearer(tg.Key)
		h["Content-Type"] = "application/json"
		gen := do(t, "POST", tg.BaseURL+"/v1/images/generations", h,
			`{"model":"dreamshaper-v8","prompt":"a red apple on a table","size":"256x256","seed":42,"steps":6,"cfg_scale":7,"negative_prompt":"blurry","response_format":"b64_json"}`)
		g := decode(t, tg.Name+" generation", gen)
		body, ct := imageForm(field("model", "dreamshaper-v8"), field("prompt", "a green apple"), field("size", "256x256"),
			field("seed", "7"), field("steps", "6"), field("strength", "0.6"), imgPart{name: "image", filename: "apple.png", file: g, isFile: true})
		eh := bearer(tg.Key)
		eh["Content-Type"] = ct
		e := decode(t, tg.Name+" edit", do(t, "POST", tg.BaseURL+"/v1/images/edits", eh, body))
		outputs[tg.Name] = [2][]byte{g, e}
	}
	if len(targets) == 2 {
		a, b := outputs[targets[0].Name], outputs[targets[1].Name]
		// Same seed, runtime build and tuning block: the same bytes.
		if !bytes.Equal(a[0], b[0]) || !bytes.Equal(a[1], b[1]) {
			t.Errorf("outputs differ: generation equal %v, edit equal %v", bytes.Equal(a[0], b[0]), bytes.Equal(a[1], b[1]))
		}
	}
}

// named is a file part with a filename (Bun types uploads by extension).
func named(field, filename string, b []byte) imgPart {
	return imgPart{name: field, filename: filename, file: b, isFile: true}
}

// TestDifferentialRetrySemantics: clients (device workers, coding agents)
// retry on 503 with Retry-After. A start that fails transiently must keep
// that contract on both gateways, every time it is asked again; a failure
// that needs someone to act answers its real 4xx without Retry-After.
func TestDifferentialRetrySemantics(t *testing.T) {
	if os.Getenv("LOCALBASE_CONTRACT_RUNTIMES") == "" {
		t.Skip("needs LOCALBASE_CONTRACT_RUNTIMES (a real sd-server that fails to load placeholder weights)")
	}
	ask := func(model string) map[string]*Resp {
		out := map[string]*Resp{}
		for _, tg := range targets {
			h := bearer(tg.Key)
			h["Content-Type"] = "application/json"
			out[tg.Name] = do(t, "POST", tg.BaseURL+"/v1/images/generations", h,
				`{"model":"`+model+`","prompt":"a","size":"256x256","response_format":"b64_json"}`)
		}
		return out
	}
	check := func(label string, rs map[string]*Resp, status int, code string, retry bool) {
		t.Helper()
		for name, r := range rs {
			got := r.JSON(t)["error"].(map[string]any)["code"]
			if r.Status != status || got != code || (r.Header.Get("Retry-After") != "") != retry {
				t.Errorf("%s %s: %d %v Retry-After=%q, want %d %s retry=%v", name, label, r.Status, got, r.Header.Get("Retry-After"), status, code, retry)
			}
		}
	}
	// Transient: the backend starts and dies on the placeholder weights.
	// Asked twice: TS keeps its failed runtime, Go retries the start.
	for i := 1; i <= 2; i++ {
		rs := ask("dreamshaper-8-lcm")
		check(fmt.Sprintf("failed start #%d", i), rs, 503, "service_unavailable", true)
		sameError(t, fmt.Sprintf("failed start #%d", i), rs)
	}
	// Permanent until someone installs it: 409, no Retry-After. A built-in
	// entry: a former one kept as a user entry (#368) is sized first and,
	// on a busy machine, refused for memory.
	// Both size the model first, so on a busy machine it is a memory 503.
	missing := ask("juggernaut-xl-v9")
	sameError(t, "files missing", missing)
	if r := missing["go"]; r != nil && r.Status == 503 && r.JSON(t)["error"].(map[string]any)["code"] == "insufficient_memory" {
		t.Log("files missing: refused for memory first on this machine; the 409 path was not reached")
	} else {
		check("files missing", missing, 409, "model_install_consent_required", false)
	}
	// A missing multi-file model is sized from the catalog first, so on a busy
	// machine it is a transient memory 503; either way both gateways agree.
	flux := ask("flux1-schnell-q4_0")
	sameError(t, "missing catalog-sized model", flux)
	if a, b := flux["ts"], flux["go"]; a != nil && b != nil && (a.Header.Get("Retry-After") == "") != (b.Header.Get("Retry-After") == "") {
		t.Errorf("Retry-After differs: ts %q, go %q", a.Header.Get("Retry-After"), b.Header.Get("Retry-After"))
	}
}
