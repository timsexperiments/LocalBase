package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// Image generation and edits (the TS imageGeneration and imageEdit routes),
// served by sd-server's OpenAI-compatible API. Tuning that sd-server reads
// from an inline <sd_cpp_extra_args> block is appended to the prompt.

var imageSizes = []string{"256x256", "512x512", "1024x1024", "512x768", "768x512", "768x1024", "1024x768"}

const (
	imageLoraMaxStrength = 2
	imageEditMaxBytes    = 20 * 1024 * 1024
	imageFileMaxBytes    = 8 * 1024 * 1024
	extraArgsMarker      = "sd_cpp_extra_args"
)

func utf16Length(s string) int { return len(utf16.Encode([]rune(s))) }

func quoteOptions(options []string) string {
	q := make([]string, len(options))
	for i, o := range options {
		q[i] = strconv.Quote(o)
	}
	return strings.Join(q, "|")
}

func (c *checker) optEnum(obj map[string]any, key string, options ...string) {
	v, present := obj[key]
	if !present {
		return
	}
	if s, ok := v.(string); !ok || !slices.Contains(options, s) {
		c.fail(key, "Invalid option: expected one of "+quoteOptions(options))
	}
}

// optInt is z.number().int().min(lo).max(hi).optional().
func (c *checker) optInt(obj map[string]any, key string, lo, hi float64) {
	v, present := obj[key]
	if !present {
		return
	}
	f, ok := number(v)
	if !ok {
		c.fail(key, expected("number", v, true))
		return
	}
	if math.IsNaN(f) {
		c.fail(key, "Invalid input: expected number, received NaN")
		return
	}
	if f != math.Trunc(f) {
		// Zod's int check aborts the remaining checks.
		c.fail(key, "Invalid input: expected int, received number")
		return
	}
	if f < lo {
		c.fail(key, "Too small: expected number to be >="+trimFloat(lo))
	}
	if f > hi {
		c.fail(key, "Too big: expected number to be <="+trimFloat(hi))
	}
}

// noExtraArgs is the refine that keeps callers from writing the tuning block.
func (c *checker) noExtraArgs(key, s string) {
	if strings.Contains(strings.ToLower(s), extraArgsMarker) {
		c.fail(key, key+" must not contain sd_cpp_extra_args.")
	}
}

// validateImageTuning checks the imageTuningSchema fields in schema order.
func (c *checker) imageTuning(obj map[string]any, withStrength bool) {
	if v, present := obj["negative_prompt"]; present {
		if s, ok := v.(string); !ok {
			c.fail("negative_prompt", expected("string", v, true))
		} else {
			if utf16Length(s) > 16_384 {
				c.fail("negative_prompt", "Too big: expected string to have <=16384 characters")
			}
			c.noExtraArgs("negative_prompt", s)
		}
	}
	c.optInt(obj, "seed", 0, 2_147_483_647)
	c.optInt(obj, "steps", 1, 150)
	c.optNumber(obj, "", "cfg_scale", 0, 30, false)
	if withStrength {
		c.optNumber(obj, "", "strength", 0, 1, false)
	}
	c.optNumber(obj, "", "lora_strength", 0, imageLoraMaxStrength, false)
}

func validateImageGeneration(req map[string]any) *checker {
	c := &checker{}
	c.imageTuning(req, false)
	if v, present := req["prompt"]; true {
		if s, ok := v.(string); !ok {
			c.fail("prompt", expected("string", v, present))
		} else {
			c.noExtraArgs("prompt", s)
		}
	}
	if _, present := req["model"]; present {
		c.modelID(req)
	}
	c.optNumber(req, "", "n", 1, 10, false)
	c.optEnum(req, "quality", "standard", "hd")
	c.optEnum(req, "response_format", "url", "b64_json")
	c.optEnum(req, "size", imageSizes...)
	c.optEnum(req, "style", "vivid", "natural")
	c.optString(req, "", "user")
	return c
}

// imageTuning is the validated tuning a request carries.
type imageTuning struct {
	negativePrompt              string
	seed, steps                 *int64
	cfgScale, strength, loraStr *float64
}

func tuningFrom(obj map[string]any) imageTuning {
	var t imageTuning
	t.negativePrompt, _ = obj["negative_prompt"].(string)
	num := func(k string) *float64 {
		if f, ok := number(obj[k]); ok {
			return &f
		}
		return nil
	}
	integer := func(k string) *int64 {
		if f := num(k); f != nil {
			n := int64(*f)
			return &n
		}
		return nil
	}
	t.seed, t.steps = integer("seed"), integer("steps")
	t.cfgScale, t.strength, t.loraStr = num("cfg_scale"), num("strength"), num("lora_strength")
	return t
}

// imageLora is the LoRA a request applies: the requested strength or the
// catalog default, none at strength 0.
type imageLora struct {
	path       string
	multiplier float64
}

func imageLoraFor(spec *catalog.Spec, modelsDir string, strength *float64) *imageLora {
	path := runtime.ImageLoraPath(spec, modelsDir)
	if path == "" {
		return nil
	}
	m := spec.ImageLaunchOptions.Lora.DefaultStrength
	if strength != nil {
		m = *strength
	}
	if m <= 0 {
		return nil
	}
	return &imageLora{path, m}
}

// promptWithTuning appends the sd_cpp_extra_args block (TS
// promptWithImageTuning, same key order).
func promptWithTuning(prompt string, t imageTuning, lora *imageLora) string {
	type kv struct {
		k string
		v any
	}
	var extra []kv
	if lora != nil {
		extra = append(extra, kv{"lora", []any{orderedJSON{{"path", lora.path}, {"multiplier", lora.multiplier}, {"is_high_noise", false}}}})
	}
	if t.negativePrompt != "" {
		extra = append(extra, kv{"negative_prompt", t.negativePrompt})
	}
	if t.seed != nil {
		extra = append(extra, kv{"seed", *t.seed})
	}
	if t.strength != nil {
		extra = append(extra, kv{"strength", *t.strength})
	}
	var sample orderedJSON
	if t.steps != nil {
		sample = append(sample, kvPair{"sample_steps", *t.steps})
	}
	if t.cfgScale != nil {
		sample = append(sample, kvPair{"guidance", orderedJSON{{"txt_cfg", *t.cfgScale}}})
	}
	if len(sample) > 0 {
		extra = append(extra, kv{"sample_params", sample})
	}
	if len(extra) == 0 {
		return prompt
	}
	o := make(orderedJSON, len(extra))
	for i, e := range extra {
		o[i] = kvPair{e.k, e.v}
	}
	// json.Marshal would HTML-escape the result; JSON.stringify does not.
	b, _ := marshalNoEscape(o)
	return prompt + " <sd_cpp_extra_args>" + string(b) + "</sd_cpp_extra_args>"
}

type kvPair struct {
	k string
	v any
}

// orderedJSON marshals as an object with its keys in order, unescaped like
// JSON.stringify.
type orderedJSON []kvPair

func (o orderedJSON) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := marshalNoEscape(p.k)
		v, err := marshalNoEscape(p.v)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func loraUnusable(w http.ResponseWriter, r *http.Request, spec *catalog.Spec, strength *float64) bool {
	if strength == nil || spec == nil || spec.Kind != "image" || runtime.ImageLoraPath(spec, "") != "" {
		return false
	}
	badRequest(w, r, fmt.Sprintf("Model '%s' has no LoRA; lora_strength applies only to image models added with one.", spec.ModelID), strp("lora_strength"))
	return true
}

// acquireImage resolves and admits an image model, writing the TS errors.
func (s *Server) acquireImage(w http.ResponseWriter, r *http.Request, model string) (*runtime.ServiceLease, bool) {
	if s.Image == nil {
		notConfigured(w, r, "Image")
		return nil, false
	}
	id, err := s.Image.Resolve(model)
	switch {
	case errors.Is(err, runtime.ErrNotConfigured):
		notConfigured(w, r, "Image")
		return nil, false
	case errors.Is(err, runtime.ErrModelNotFound):
		modelNotFound(w, r, model)
		return nil, false
	}
	lease, err := s.Image.Acquire(r.Context(), id)
	if err != nil {
		s.logAdmissionRejection(w.Header().Get("X-Localbase-Request-Id"), "image", id, err)
		runtimeError(w, r, err, id, "Image")
		return nil, false
	}
	return lease, true
}

func (s *Server) imageTelemetry(w http.ResponseWriter, lease *runtime.ServiceLease, started time.Time) *inference {
	return &inference{s: s, modality: "image", modelID: lease.ModelID, spec: s.Catalog.ByID(lease.ModelID),
		requestID: w.Header().Get("X-Localbase-Request-Id"), started: started, queueWait: lease.Waited, active: 1, slots: 1}
}

func (s *Server) imageGeneration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, "POST")
		return
	}
	started := time.Now()
	req, ok := readJSON(w, r)
	if !ok {
		return
	}
	if c := validateImageGeneration(req); !c.ok() {
		c.reply(w, r)
		return
	}
	tuning := tuningFrom(req)
	model, _ := req["model"].(string)
	target := model
	if target == "" {
		target = s.activeImageModel()
	}
	if loraUnusable(w, r, s.Catalog.ByID(target), tuning.loraStr) {
		return
	}
	lease, ok := s.acquireImage(w, r, model)
	if !ok {
		return
	}
	defer lease.Release()
	for _, k := range []string{"negative_prompt", "seed", "steps", "cfg_scale", "lora_strength"} {
		delete(req, k)
	}
	req["prompt"] = promptWithTuning(req["prompt"].(string), tuning,
		imageLoraFor(s.Catalog.ByID(lease.ModelID), lease.Target.Dir, tuning.loraStr))
	body, _ := marshalNoEscape(req)
	s.relay(w, r, lease.BaseURL+"/v1/images/generations", body, lease.ModelID, inferOpts{kind: "image", interrupted: lease.Interrupted}, s.imageTelemetry(w, lease, started))
}

func (s *Server) activeImageModel() string {
	if s.LLM == nil {
		return ""
	}
	return s.LLM.Config().Active["image"]
}

type editFile struct {
	data        []byte
	name, ctype string
}

func (s *Server) imageEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, "POST")
		return
	}
	started := time.Now()
	if r.ContentLength > imageEditMaxBytes {
		payloadTooLargeLimit(w, r, imageEditMaxBytes)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, imageEditMaxBytes+1))
	if err == nil && len(body) > imageEditMaxBytes {
		payloadTooLargeLimit(w, r, imageEditMaxBytes)
		return
	}
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data;") {
		badRequest(w, r, "Expected multipart/form-data.", strp("content-type"))
		return
	}
	form, ferr := readMultipart(r, body)
	if err != nil || ferr != nil {
		badRequest(w, r, "Invalid multipart image edit request.", nil)
		return
	}
	text := func(name string) (string, bool) {
		if v := form[name]; len(v) > 0 && !v[0].isFile {
			return v[0].value, true
		}
		return "", false
	}
	present := func(name string) bool { return len(form[name]) > 0 }
	bad := func(msg, param string) { badRequest(w, r, msg, strp(param)) }

	promptValue, _ := text("prompt")
	prompt := strings.TrimFunc(promptValue, isJSWhitespace)
	if prompt == "" || utf16Length(prompt) > 16_384 {
		bad("prompt is required and must not exceed 16384 characters.", "prompt")
		return
	}
	if strings.Contains(strings.ToLower(prompt), extraArgsMarker) {
		bad("prompt must not contain sd_cpp_extra_args.", "prompt")
		return
	}
	modelID, _ := text("model")
	if modelID == "" {
		modelID = s.activeImageModel()
	}
	if modelID == "" {
		notConfigured(w, r, "Image")
		return
	}
	spec := s.Catalog.ByID(modelID)
	if spec == nil || spec.Kind != "image" {
		modelNotFound(w, r, modelID)
		return
	}
	if !s.capabilities(r.Context(), spec).Has("image-to-image") {
		bad(fmt.Sprintf("Model '%s' does not support image-to-image edits.", modelID), "model")
		return
	}
	var images []editFile
	for _, name := range []string{"image[]", "image"} {
		for _, p := range form[name] {
			if p.isFile {
				images = append(images, editFile{p.file, p.filename, p.ctype})
			}
		}
	}
	var mask *editFile
	if m := form["mask"]; len(m) > 0 {
		if !m[0].isFile || len(m[0].file) == 0 {
			bad("mask must be a nonempty image file.", "mask")
			return
		}
		mask = &editFile{m[0].file, m[0].filename, m[0].ctype}
	}
	if len(images) < 1 || len(images) > 4 {
		bad("Provide between 1 and 4 image files using image or image[].", "image")
		return
	}
	n := 1.0
	if present("n") {
		v, _ := text("n")
		n = jsNumber(v)
	}
	if n != math.Trunc(n) || n < 1 || n > 10 {
		bad("n must be an integer from 1 to 10.", "n")
		return
	}
	size, _ := text("size")
	if size != "" && !slices.Contains(imageSizes, size) {
		bad("size must be one of "+strings.Join(imageSizes, ", ")+".", "size")
		return
	}
	if present("response_format") {
		if v, ok := text("response_format"); !ok || (v != "url" && v != "b64_json") {
			bad("response_format must be url or b64_json.", "response_format")
			return
		}
	}
	// Tuning fields go through the same schema as JSON numbers.
	tuningObj := map[string]any{}
	if v, ok := text("negative_prompt"); ok && strings.TrimFunc(v, isJSWhitespace) != "" {
		tuningObj["negative_prompt"] = v
	}
	for _, k := range []string{"seed", "steps", "cfg_scale", "strength", "lora_strength"} {
		if v, ok := text(k); ok && strings.TrimFunc(v, isJSWhitespace) != "" {
			tuningObj[k] = jsNumber(v)
		}
	}
	c := &checker{}
	c.imageTuning(tuningObj, true)
	if !c.ok() {
		param := c.issues[0].path
		if param == "strength" {
			bad("strength must be between 0 and 1.", param)
		} else {
			bad(param+" is invalid: "+c.issues[0].msg+".", param)
		}
		return
	}
	tuning := tuningFrom(tuningObj)
	if loraUnusable(w, r, spec, tuning.loraStr) {
		return
	}
	outputFormat := "png"
	if present("output_format") {
		v, ok := text("output_format")
		if !ok || (v != "png" && v != "jpeg") {
			bad("output_format must be png or jpeg.", "output_format")
			return
		}
		outputFormat = v
	}
	files := images
	if mask != nil {
		files = append(append([]editFile{}, images...), *mask)
	}
	for i, f := range files {
		param := "image"
		if mask != nil && i == len(files)-1 {
			param = "mask"
		}
		if len(f.data) == 0 || len(f.data) > imageFileMaxBytes {
			bad("Each image file must be between 1 byte and 8 MiB.", param)
			return
		}
		mime := imageMimeFromMagic(f.data)
		if mime == "" {
			bad("Images must be PNG or JPEG by file signature.", param)
			return
		}
		if f.ctype != "" && f.ctype != mime {
			bad("Image content type does not match its file signature.", param)
			return
		}
		var d *imageDims
		if mime == "image/png" {
			d = validatePNG(f.data)
		} else {
			d = validateJPEG(f.data)
		}
		if d == nil || d.width < 1 || d.height < 1 || d.width > 2048 || d.height > 2048 || d.width*d.height > 4_194_304 {
			bad("Images must have valid dimensions no larger than 2048x2048 (4.2 megapixels).", param)
			return
		}
	}
	lease, ok := s.acquireImage(w, r, modelID)
	if !ok {
		return
	}
	defer lease.Release()
	var out bytes.Buffer
	mw := multipart.NewWriter(&out)
	mw.WriteField("prompt", promptWithTuning(prompt, tuning, imageLoraFor(spec, lease.Target.Dir, tuning.loraStr)))
	mw.WriteField("n", strconv.Itoa(int(n)))
	mw.WriteField("output_format", outputFormat)
	if size != "" {
		mw.WriteField("size", size)
	}
	part := func(field string, f editFile, fallback string) {
		name := f.name
		if name == "" {
			name = fallback
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, field, name))
		ct := f.ctype
		if ct == "" {
			ct = "application/octet-stream"
		}
		h.Set("Content-Type", ct)
		pw, _ := mw.CreatePart(h)
		pw.Write(f.data)
	}
	for _, img := range images {
		part("image[]", img, "image.png")
	}
	if mask != nil {
		part("mask", *mask, "mask.png")
	}
	mw.Close()
	s.relay(w, r, lease.BaseURL+"/v1/images/edits", out.Bytes(), lease.ModelID, inferOpts{kind: "image", contentType: mw.FormDataContentType(), interrupted: lease.Interrupted},
		s.imageTelemetry(w, lease, started))
}

// isJSWhitespace is the String.prototype.trim set.
func isJSWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// jsNumber is JavaScript's Number(string) for form fields: trimmed decimal
// (or hex/binary/octal) text, "" as 0, anything else NaN.
func jsNumber(s string) float64 {
	t := strings.TrimFunc(s, isJSWhitespace)
	if t == "" {
		return 0
	}
	if len(t) > 2 && t[0] == '0' && strings.ContainsRune("xXbBoO", rune(t[1])) {
		base := map[byte]int{'x': 16, 'X': 16, 'b': 2, 'B': 2, 'o': 8, 'O': 8}[t[1]]
		if n, err := strconv.ParseUint(t[2:], base, 64); err == nil {
			return float64(n)
		}
		return math.NaN()
	}
	switch t {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	for _, c := range t {
		if !strings.ContainsRune("0123456789+-.eE", c) {
			return math.NaN()
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		var ne *strconv.NumError
		if errors.As(err, &ne) && errors.Is(ne.Err, strconv.ErrRange) {
			return f
		}
		return math.NaN()
	}
	return f
}

func payloadTooLargeLimit(w http.ResponseWriter, r *http.Request, limit int) {
	w.Header().Set("Connection", "close")
	writeError(w, r, 413, "invalid_request_error", "payload_too_large",
		fmt.Sprintf("Request body exceeds the %d MiB limit.", limit/(1024*1024)), nil)
}
