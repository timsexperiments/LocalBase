package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// Speech generation: POST /v1/audio/speech, the TS contract. tts-server
// streams raw PCM16 from /v1/generate; CrispASR answers its own
// /v1/audio/speech. The gateway returns WAV or PCM, or streams them as audio
// or server-sent events.

const (
	speechMaxInputChars = 256
	speechMaxPCMBytes   = runtime.SpeechMaxFrames * 1920 * 2
	speechPCMType       = "audio/pcm;rate=24000;channels=1;encoding=s16le"
	speechTimeout       = 120 * time.Second
)

var (
	builtInVoices = []string{"default", "harbor", "willow", "af_heart", "af_bella"}
	customVoiceRe = regexp.MustCompile(`^voice-[0-9a-f]{12}$`)
)

type speechRequest struct {
	Model          string
	Input          string
	Voice          string
	ResponseFormat string
	StreamFormat   string
}

// validateSpeech applies the TS zod schema: strict keys, input 1-256 code
// points, a built-in voice or a custom voice id, wav|pcm, audio|sse, speed 1.
func validateSpeech(body []byte) (*speechRequest, []string, *string) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return nil, []string{": Invalid input: expected object, received " + jsonKind(body)}, nil
	}
	var issues []string
	var param *string
	str := func(k string) (string, bool) {
		v, ok := raw[k]
		if !ok {
			return "", false
		}
		var s string
		if json.Unmarshal(v, &s) != nil {
			issues = append(issues, k+": Invalid input: expected string")
			return "", true
		}
		return s, true
	}
	req := &speechRequest{ResponseFormat: "wav"}
	if m, ok := str("model"); !ok {
		issues = append(issues, "model: Invalid input: expected string, received undefined")
	} else if strings.TrimSpace(m) == "" {
		issues = append(issues, "model: model must not be empty")
	} else {
		req.Model = m
	}
	if in, ok := str("input"); !ok {
		issues = append(issues, "input: Invalid input: expected string, received undefined")
	} else {
		switch n := utf8.RuneCountInString(in); {
		case n == 0:
			issues = append(issues, "input: input must not be empty")
		case n > speechMaxInputChars:
			issues = append(issues, fmt.Sprintf("input: input must not exceed %d characters", speechMaxInputChars))
		}
		req.Input = in
	}
	if v, ok := str("voice"); !ok {
		issues = append(issues, "voice: Invalid input: expected string, received undefined")
	} else if !slices.Contains(builtInVoices, v) && !customVoiceRe.MatchString(v) {
		issues = append(issues, "voice: voice must be one of "+strings.Join(builtInVoices, ", ")+" or a custom voice id")
	} else {
		req.Voice = v
	}
	if f, ok := str("response_format"); ok {
		if f != "wav" && f != "pcm" {
			issues = append(issues, "response_format: response_format supports only 'wav' or 'pcm'")
			param = strp("response_format")
		} else {
			req.ResponseFormat = f
		}
	}
	if f, ok := str("stream_format"); ok {
		if f != "audio" && f != "sse" {
			issues = append(issues, "stream_format: stream_format supports only 'audio' or 'sse'")
		} else {
			req.StreamFormat = f
		}
	}
	if v, ok := raw["speed"]; ok && strings.TrimSpace(string(v)) != "1" {
		issues = append(issues, "speed: speed must be 1")
	}
	if _, ok := raw["instructions"]; ok {
		issues = append(issues, "instructions: instructions are not supported")
	}
	for k := range raw {
		if !slices.Contains([]string{"model", "input", "voice", "response_format", "stream_format", "speed", "instructions"}, k) {
			issues = append(issues, `: Unrecognized key: "`+k+`"`)
		}
	}
	return req, issues, param
}

func (s *Server) speech(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, "POST")
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if !json.Valid(body) {
		badRequest(w, r, "Invalid JSON payload.", nil)
		return
	}
	req, issues, param := validateSpeech(body)
	if len(issues) > 0 {
		badRequest(w, r, "Validation failed: "+strings.Join(issues, ", "), param)
		return
	}
	if spec := s.Catalog.ByID(req.Model); spec != nil && spec.Kind == "tts" && !ttsVoiceAllowed(spec.TTSRuntime, req.Voice) {
		badRequest(w, r, fmt.Sprintf(`Voice "%s" is not supported by model "%s".`, req.Voice, req.Model), nil)
		return
	}
	if customVoiceRe.MatchString(req.Voice) && !s.customVoiceExists(r, req.Voice) {
		writeError(w, r, 400, "invalid_request_error", "voice_not_found",
			"The requested custom voice does not exist. It may have been deleted; list voices at /_localbase/voices.", strp("voice"))
		return
	}
	if s.TTS == nil {
		notConfigured(w, r, "TTS")
		return
	}
	id, err := s.TTS.Resolve(req.Model)
	switch {
	case errors.Is(err, runtime.ErrNotConfigured):
		notConfigured(w, r, "TTS")
		return
	case errors.Is(err, runtime.ErrModelNotFound):
		modelNotFound(w, r, req.Model)
		return
	}
	lease, err := s.TTS.Acquire(r.Context(), id)
	if err != nil {
		runtimeError(w, r, err, id)
		return
	}
	defer lease.Release()
	pcm, err := s.generateSpeech(r, lease, req)
	if err != nil {
		var voice *voiceUnavailable
		switch {
		case r.Context().Err() != nil:
			w.WriteHeader(499)
		case errors.As(err, &voice):
			writeError(w, r, 400, "invalid_request_error", "voice_not_supported", voice.Error(), strp("voice"))
		default:
			upstreamFailure(w, r, err.Error())
		}
		return
	}
	writeSpeech(w, req, pcm)
}

type voiceUnavailable struct{ voice, backend string }

func (e *voiceUnavailable) Error() string {
	return fmt.Sprintf(`Voice "%s" is not available with the %s backend selected for this model on this device.`, e.voice, e.backend)
}

// generateSpeech returns PCM16 mono 24 kHz samples.
func (s *Server) generateSpeech(r *http.Request, lease *runtime.ServiceLease, req *speechRequest) ([]byte, error) {
	ctx := r.Context()
	t := lease.Target
	client := &http.Client{Timeout: speechTimeout}
	switch t.Backend.Adapter {
	case "crispasr/v1":
		payload := map[string]any{"input": req.Input, "response_format": "pcm"}
		if req.Voice != "default" {
			file := t.VoiceFile(req.Voice)
			if file == "" {
				return nil, &voiceUnavailable{req.Voice, t.Backend.Name}
			}
			payload["voice"] = file
		}
		b, _ := json.Marshal(payload)
		hr, _ := http.NewRequestWithContext(ctx, http.MethodPost, lease.BaseURL+"/v1/audio/speech", bytes.NewReader(b))
		hr.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(hr)
		if err != nil {
			return nil, errors.New("Speech runtime request failed.")
		}
		defer resp.Body.Close()
		pcm, _ := io.ReadAll(io.LimitReader(resp.Body, speechMaxPCMBytes*4+1))
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("Speech runtime rejected the request (HTTP %d).", resp.StatusCode)
		}
		if len(pcm) == 0 || len(pcm)%2 != 0 {
			return nil, errors.New("Speech runtime returned an unexpected audio format.")
		}
		return pcm, nil
	case "tts-server/v1":
		b, _ := json.Marshal(map[string]string{"input": req.Input, "voice": req.Voice})
		for {
			hr, _ := http.NewRequestWithContext(ctx, http.MethodPost, lease.BaseURL+"/v1/generate", bytes.NewReader(b))
			hr.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(hr)
			if err != nil {
				return nil, errors.New("Speech runtime request failed.")
			}
			if resp.StatusCode == 503 {
				// A cancelled generation finishes its window before the slot frees.
				resp.Body.Close()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return nil, fmt.Errorf("Speech runtime rejected the request (HTTP %d).", resp.StatusCode)
			}
			if resp.Header.Get("X-Audio-Format") != "s16le" || resp.Header.Get("X-Sample-Rate") != strconv.Itoa(runtime.SpeechSampleRate) {
				return nil, errors.New("Speech runtime returned an unexpected audio format.")
			}
			pcm, _ := io.ReadAll(io.LimitReader(resp.Body, speechMaxPCMBytes+1))
			switch {
			case len(pcm) > speechMaxPCMBytes:
				return nil, errors.New("Speech output exceeds the native frame limit.")
			case len(pcm) == 0 || len(pcm)%(1920*2) != 0:
				return nil, errors.New("Speech runtime returned audio that is not a whole number of frames.")
			}
			return pcm, nil
		}
	}
	return nil, fmt.Errorf("%s does not generate speech in this LocalBase build", t.Backend.Name)
}

// wavHeader is the 44-byte PCM16 mono 24 kHz header (size -1: streaming).
func wavHeader(dataBytes int64) []byte {
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	copy(h[8:], "WAVEfmt ")
	copy(h[36:], "data")
	size := uint32(0xffffffff)
	riff := uint32(0xffffffff)
	if dataBytes >= 0 {
		size, riff = uint32(dataBytes), uint32(36+dataBytes)
	}
	binary.LittleEndian.PutUint32(h[4:], riff)
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], runtime.SpeechSampleRate)
	binary.LittleEndian.PutUint32(h[28:], runtime.SpeechSampleRate*2)
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	binary.LittleEndian.PutUint32(h[40:], size)
	return h
}

func writeSpeech(w http.ResponseWriter, req *speechRequest, pcm []byte) {
	switch req.StreamFormat {
	case "":
		body := pcm
		ctype := speechPCMType
		if req.ResponseFormat == "wav" {
			body, ctype = append(wavHeader(int64(len(pcm))), pcm...), "audio/wav"
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(200)
		w.Write(body)
	case "audio":
		ctype := speechPCMType
		if req.ResponseFormat == "wav" {
			ctype = "audio/wav"
		}
		w.Header().Set("Content-Type", ctype)
		w.WriteHeader(200)
		if req.ResponseFormat == "wav" {
			w.Write(wavHeader(-1))
		}
		w.Write(pcm)
	case "sse":
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		event := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		if req.ResponseFormat == "wav" {
			event(map[string]string{"type": "speech.audio.delta", "audio": base64.StdEncoding.EncodeToString(wavHeader(-1))})
		}
		event(map[string]string{"type": "speech.audio.delta", "audio": base64.StdEncoding.EncodeToString(pcm)})
		event(map[string]string{"type": "speech.audio.done"})
	}
}

// customVoiceExists: a stored voice whose recording still matches its
// checksum (TS voiceStore.resolve).
func (s *Server) customVoiceExists(r *http.Request, id string) bool {
	if s.Voices == nil {
		return false
	}
	p, err := s.Voices.Resolve(r.Context(), id)
	return err == nil && p != ""
}

// readBody reads a bounded request body (25 MiB, TS MAX_REQUEST_BYTES).
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.ContentLength > maxRequestBytes {
		payloadTooLarge(w, r)
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		if r.Context().Err() != nil {
			w.WriteHeader(499)
			return nil, false
		}
		badRequest(w, r, "Invalid JSON payload.", nil)
		return nil, false
	}
	if len(body) > maxRequestBytes {
		payloadTooLarge(w, r)
		return nil, false
	}
	return body, true
}

// ttsVoiceAllowed is the TS catalog rule: llama-tts models take default,
// custom voices and their reference voices; others their listed voices.
func ttsVoiceAllowed(rt *catalog.TTSRuntime, voice string) bool {
	if rt == nil {
		return false
	}
	if rt.Kind == "llama-tts" {
		if voice == "default" || customVoiceRe.MatchString(voice) {
			return true
		}
		for _, v := range rt.ReferenceVoices {
			if v.Name == voice {
				return true
			}
		}
		return false
	}
	for _, v := range rt.Voices {
		if v.Name == voice {
			return true
		}
	}
	return false
}

// jsonKind names a JSON value's type as zod reports it.
func jsonKind(b []byte) string {
	t := bytes.TrimSpace(b)
	switch {
	case len(t) == 0:
		return "undefined"
	case t[0] == '[':
		return "array"
	case t[0] == '"':
		return "string"
	case t[0] == 't' || t[0] == 'f':
		return "boolean"
	case t[0] == 'n':
		return "null"
	}
	return "number"
}
