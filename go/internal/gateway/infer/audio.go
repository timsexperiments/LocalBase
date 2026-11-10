package infer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// Speech recognition: POST /v1/audio/transcriptions and /translations, the
// TS transcription contract (multipart form, whisper.cpp behind it).

var transcriptionFormats = []string{"json", "text", "srt", "verbose_json", "vtt"}

func (h *Handler) Transcription(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if r.ContentLength > httpx.MaxRequestBytes {
		httpx.PayloadTooLarge(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, httpx.MaxRequestBytes+1))
	if err != nil {
		httpx.BadRequest(w, r, "Invalid form data payload.", nil)
		return
	}
	if len(body) > httpx.MaxRequestBytes {
		httpx.PayloadTooLarge(w, r)
		return
	}
	form, err := httpx.ReadMultipart(r, body)
	if err != nil {
		httpx.BadRequest(w, r, "Invalid form data payload.", nil)
		return
	}
	// The TS zod schema: file (a Blob), optional model, language, prompt,
	// include[], response_format, temperature (0-1), timestamp_granularities[].
	var issues []string
	param := (*string)(nil)
	single := func(name string) (string, bool) {
		v := form[name]
		if len(v) == 0 {
			return "", false
		}
		if len(v) > 1 || v[0].IsFile {
			issues = append(issues, name+": Invalid input: expected string, received "+map[bool]string{true: "File", false: "array"}[v[0].IsFile])
			return "", true
		}
		return v[0].Value, true
	}
	file := form["file"]
	if len(file) != 1 || !file[0].IsFile {
		issues = append(issues, "file: file must be a valid File or Blob object")
	}
	model, hasModel := single("model")
	if hasModel && strings.TrimSpace(model) == "" {
		issues = append(issues, "model: model must not be empty")
	}
	single("language")
	single("prompt")
	format := "json"
	if v, ok := single("response_format"); ok {
		if !slices.Contains(transcriptionFormats, v) {
			issues = append(issues, `response_format: Invalid option: expected one of "json"|"text"|"srt"|"verbose_json"|"vtt"`)
			param = httpx.StrPtr("response_format")
		} else {
			format = v
		}
	}
	if v, ok := single("temperature"); ok && strings.TrimSpace(v) != "" {
		t, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		switch {
		case err != nil:
			issues = append(issues, "temperature: Invalid input: expected number, received NaN")
		case t < 0:
			issues = append(issues, "temperature: Too small: expected number to be >=0")
		case t > 1:
			issues = append(issues, "temperature: Too big: expected number to be <=1")
		}
	}
	for _, g := range form["timestamp_granularities[]"] {
		if g.IsFile || (g.Value != "word" && g.Value != "segment") {
			issues = append(issues, `timestamp_granularities.0: Invalid option: expected one of "word"|"segment"`)
		}
	}
	for _, name := range []string{"include[]"} {
		for _, v := range form[name] {
			if v.IsFile {
				issues = append(issues, "include.0: Invalid input: expected string, received File")
			}
		}
	}
	if len(issues) > 0 {
		httpx.BadRequest(w, r, "Validation failed: "+strings.Join(issues, ", "), param)
		return
	}
	if h.opts.Runtimes.STT == nil {
		httpx.NotConfigured(w, r, "STT")
		return
	}
	id, err := h.opts.Runtimes.STT.Resolve(model)
	switch {
	case errors.Is(err, runtime.ErrNotConfigured):
		httpx.NotConfigured(w, r, "STT")
		return
	case errors.Is(err, runtime.ErrModelNotFound):
		httpx.ModelNotFound(w, r, model)
		return
	}
	httpx.RunHooksFrom(r.Context()).Queue()
	lease, err := h.opts.Runtimes.STT.Acquire(r.Context(), id)
	if err != nil {
		httpx.RuntimeError(w, r, err, id, "STT")
		return
	}
	httpx.RunHooksFrom(r.Context()).Admit()
	defer lease.Release()
	tel := &inference{h: h, modality: "stt", modelID: lease.ModelID, spec: h.opts.Catalog.ByID(lease.ModelID),
		requestID: w.Header().Get("X-Localbase-Request-Id"), started: started, queueWait: lease.Waited, active: 1, slots: 1}
	tel.begin(r.Context(), r.Method)

	// Forward the validated fields; whisper-server has one /inference
	// endpoint, and translation is a flag.
	var out bytes.Buffer
	mw := multipart.NewWriter(&out)
	for _, name := range []string{"language", "prompt", "response_format", "temperature"} {
		if v := form[name]; len(v) == 1 {
			mw.WriteField(name, v[0].Value)
		}
	}
	for _, name := range []string{"include[]", "timestamp_granularities[]"} {
		for _, v := range form[name] {
			mw.WriteField(name, v.Value)
		}
	}
	if model != "" {
		mw.WriteField("model", model)
	}
	if strings.HasSuffix(r.URL.Path, "/translations") {
		mw.WriteField("translate", "true")
	}
	fw, _ := mw.CreateFormFile("file", file[0].Filename)
	fw.Write(file[0].File)
	mw.Close()
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, lease.BaseURL+"/inference", &out)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := h.backendCall(tel.ctx, r, req, &http.Client{Timeout: 10 * time.Minute})
	if err != nil {
		if r.Context().Err() != nil {
			w.WriteHeader(499)
			tel.finish("cancelled", 499, "request_aborted")
			return
		}
		if lease.Interrupted() {
			memoryInterrupted(w, r, tel)
			return
		}
		backendGone(w, r, tel, "STT")
		return
	}
	tel.upstreamStatus = resp.StatusCode
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if (readErr != nil || resp.StatusCode < 200 || resp.StatusCode > 299) && lease.Interrupted() {
		memoryInterrupted(w, r, tel)
		return
	}
	if readErr != nil {
		if r.Context().Err() != nil {
			w.WriteHeader(499)
			tel.finish("cancelled", 499, "request_aborted")
			return
		}
		backendGone(w, r, tel, "STT")
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		httpx.UpstreamFailure(w, r, fmt.Sprintf("The upstream service failed with HTTP %d.", resp.StatusCode))
		tel.finish("error", 502, "")
		return
	}
	switch format {
	case "text", "srt", "vtt":
		if looksLikeBackendError(resp, raw) {
			httpx.UpstreamFailure(w, r, "The upstream service returned an invalid response.")
			tel.finish("error", 502, "response_validation")
			return
		}
		text := string(raw)
		if format == "text" {
			text = normalizeTranscriptText(text)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(resp.StatusCode)
		io.WriteString(w, text)
		tel.finish("completed", resp.StatusCode, "")
		return
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil || !validTranscription(data) {
		httpx.UpstreamFailure(w, r, "The upstream service returned an invalid response.")
		tel.finish("error", 502, "response_validation")
		return
	}
	httpx.JSON(w, resp.StatusCode, normalizeTranscriptionJSON(data, format))
	tel.finish("completed", resp.StatusCode, "")
}

var whitespaceRe = regexp.MustCompile(`\s+`)

func normalizeTranscriptText(s string) string {
	return strings.TrimSpace(whitespaceRe.ReplaceAllString(s, " "))
}

func looksLikeBackendError(resp *http.Response, body []byte) bool {
	if !strings.Contains(resp.Header.Get("Content-Type"), "json") {
		return false
	}
	var m map[string]any
	t := bytes.TrimLeft(body, " \t\r\n")
	if len(t) == 0 || t[0] != '{' || json.Unmarshal(t, &m) != nil {
		return false
	}
	_, ok := m["error"]
	return ok
}

// validTranscription mirrors transcriptionResponseSchema: text is a string,
// segments (if any) have numeric ids and string text.
func validTranscription(d map[string]any) bool {
	if _, ok := d["text"].(string); !ok {
		return false
	}
	if segs, present := d["segments"]; present {
		list, ok := segs.([]any)
		if !ok {
			return false
		}
		for _, s := range list {
			m, ok := s.(map[string]any)
			if !ok {
				return false
			}
			if _, ok := m["id"].(float64); !ok {
				return false
			}
			if _, ok := m["text"].(string); !ok {
				return false
			}
		}
	}
	return true
}

func normalizeTranscriptionJSON(d map[string]any, format string) map[string]any {
	text := normalizeTranscriptText(d["text"].(string))
	if format != "verbose_json" {
		return map[string]any{"text": text}
	}
	out := map[string]any{}
	for k, v := range d {
		out[k] = v
	}
	out["text"] = text
	if segs, ok := d["segments"].([]any); ok {
		norm := make([]any, 0, len(segs))
		for _, s := range segs {
			m := map[string]any{}
			for k, v := range s.(map[string]any) {
				m[k] = v
			}
			if _, ok := m["seek"].(float64); !ok {
				if start, ok := m["start"].(float64); ok {
					m["seek"] = float64(int64(start*100 + 0.5))
				} else {
					m["seek"] = 0.0
				}
			}
			for _, k := range []string{"temperature", "avg_logprob", "compression_ratio", "no_speech_prob"} {
				if _, ok := m[k].(float64); !ok {
					m[k] = 0.0
				}
			}
			m["text"] = normalizeTranscriptText(m["text"].(string))
			norm = append(norm, m)
		}
		out["segments"] = norm
	}
	return out
}
