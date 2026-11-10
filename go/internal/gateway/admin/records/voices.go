package records

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
)

// Custom voice profiles (the TS voice-http contract):
//
//	GET    /_localbase/voices             models:read    list, limits, consent statement
//	POST   /_localbase/voices             models:manage  multipart name, consent=true, audio (WAV)
//	GET    /_localbase/voices/{id}        models:read
//	PATCH  /_localbase/voices/{id}        models:manage  {"name": ...}
//	DELETE /_localbase/voices/{id}        models:manage
//	GET    /_localbase/voices/{id}/audio  models:manage  the normalized reference WAV
//
// Voices are model inputs stored under the root, so they follow the model
// permissions. Ids are never decoded; anything else is an unmatched route.

// VoiceMultipartOverhead is what a voice upload's form may add to the audio's own limit
// (voices.MaxUploadBytes): the body is refused past the two together.
const VoiceMultipartOverhead = 16 * 1024

func voiceError(w http.ResponseWriter, r *http.Request, err error) bool {
	var ve *voices.Error
	if !errors.As(err, &ve) {
		return false
	}
	status, param := 400, httpx.StrPtr("audio")
	switch ve.Code {
	case "voice_not_found":
		status, param = 404, nil
	case "voice_name_taken", "voice_limit_reached":
		status, param = 409, httpx.StrPtr("name")
	case "audio_too_large":
		status = 413
	case "consent_required":
		param = httpx.StrPtr("consent")
	case "invalid_voice_name":
		param = httpx.StrPtr("name")
	}
	httpx.Error(w, r, status, "invalid_request_error", ve.Code, ve.Message, param)
	return true
}

func (h *Handler) VoicesAPI(w http.ResponseWriter, r *http.Request) {
	kind, id := "collection", ""
	if rest, ok := strings.CutPrefix(r.URL.Path, "/_localbase/voices/"); ok {
		parts := strings.Split(rest, "/")
		switch {
		case len(parts) == 1 && voices.IDPattern.MatchString(parts[0]):
			kind, id = "voice", parts[0]
		case len(parts) == 2 && parts[1] == "audio" && voices.IDPattern.MatchString(parts[0]):
			kind, id = "audio", parts[0]
		default:
			h.opts.Unmatched(w, r)
			return
		}
	} else if r.URL.Path != "/_localbase/voices" {
		h.opts.Unmatched(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	p := h.opts.Caller(r)
	if !p.Authenticated() {
		httpx.Unauthorized(w, r)
		return
	}
	if h.opts.Voices == nil {
		httpx.NotConfigured(w, r, "Voices")
		return
	}
	canManage, canRead := p.Can("models:manage"), p.Can("models:read")
	ctx := r.Context()
	fail := func(err error) {
		if !voiceError(w, r, err) {
			h.opts.Log.Error("Voice operation failed.", "event", "voices.failed", "error", err.Error())
			httpx.ServiceUnavailable(w, r, "Voices")
		}
	}
	public := func(v *voices.Voice) voices.Voice {
		if canManage {
			return *v
		}
		return v.Public()
	}

	switch kind {
	case "collection":
		if r.Method == http.MethodGet {
			if !canRead {
				httpx.Forbidden(w, r)
				return
			}
			list, err := h.opts.Voices.List(ctx)
			if err != nil {
				fail(err)
				return
			}
			data := make([]voices.Voice, 0, len(list))
			for i := range list {
				data = append(data, public(&list[i]))
			}
			httpx.JSON(w, 200, map[string]any{"object": "list", "data": data, "canManage": canManage,
				"limits": map[string]any{"minSeconds": voices.MinSeconds, "maxSeconds": voices.MaxSeconds,
					"maxUploadBytes": voices.MaxUploadBytes, "maxVoices": voices.Limit},
				"consentStatement": voices.ConsentStatement})
			return
		}
		if r.Method != http.MethodPost {
			// Checked here, not by the route table's gate: the method allowed depends on the path below the route.
			httpx.MethodNotAllowed(w, r, "GET, POST")
			return
		}
		if !canManage {
			httpx.Forbidden(w, r)
			return
		}
		name, consent, audio, ok := h.parseVoiceCreate(w, r)
		if !ok {
			return
		}
		v, err := h.opts.Voices.Create(ctx, name, consent, audio, p.OwnerID())
		if err != nil {
			fail(err)
			return
		}
		h.logVoice("created", v)
		httpx.JSON(w, 201, v)

	case "audio":
		if r.Method != http.MethodGet {
			// Checked here, not by the route table's gate: the method allowed depends on the path below the route.
			httpx.MethodNotAllowed(w, r, "GET")
			return
		}
		// The reference is a recording of a person; only managers replay it.
		if !canManage {
			httpx.Forbidden(w, r)
			return
		}
		path, err := h.opts.Voices.Resolve(ctx, id)
		if err != nil {
			fail(err)
			return
		}
		b, rerr := os.ReadFile(path)
		if path == "" || rerr != nil {
			voiceError(w, r, &voices.Error{Code: "voice_not_found", Message: "Voice not found."})
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(200)
		w.Write(b)

	default:
		switch r.Method {
		case http.MethodGet:
			if !canRead {
				httpx.Forbidden(w, r)
				return
			}
			v, err := h.opts.Voices.Get(ctx, id)
			if err != nil {
				fail(err)
				return
			}
			if v == nil {
				voiceError(w, r, &voices.Error{Code: "voice_not_found", Message: "Voice not found."})
				return
			}
			httpx.JSON(w, 200, public(v))
		case http.MethodPatch:
			if !canManage {
				httpx.Forbidden(w, r)
				return
			}
			name, ok := parseVoiceRename(w, r)
			if !ok {
				return
			}
			v, err := h.opts.Voices.Rename(ctx, id, name)
			if err != nil {
				fail(err)
				return
			}
			h.logVoice("renamed", v)
			httpx.JSON(w, 200, v)
		case http.MethodDelete:
			if !canManage {
				httpx.Forbidden(w, r)
				return
			}
			v, err := h.opts.Voices.Remove(ctx, id)
			if err != nil {
				fail(err)
				return
			}
			h.logVoice("deleted", v)
			httpx.JSON(w, 200, map[string]any{"id": v.ID, "object": "localbase.voice", "deleted": true})
		default:
			// Checked here, not by the route table's gate: the method allowed depends on the path below the route.
			httpx.MethodNotAllowed(w, r, "GET, PATCH, DELETE")
		}
	}
}

// logVoice records a change by id only: display names can be personal
// names, and audio never leaves the store.
func (h *Handler) logVoice(action string, v *voices.Voice) {
	attrs := []any{"event", "voices." + action, "category", "gateway", "component", "voices", "runtime", "tts", "voice_id", v.ID}
	if action == "created" {
		attrs = append(attrs, "duration_ms", v.DurationMs)
	}
	h.opts.Log.Log(context.Background(), slog.LevelInfo, "Custom voice "+action+".", attrs...)
}

func (h *Handler) parseVoiceCreate(w http.ResponseWriter, r *http.Request) (name string, consent bool, audio []byte, ok bool) {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data;") {
		httpx.Error(w, r, 415, "invalid_request_error", "unsupported_media_type", "Send multipart/form-data with name, consent and audio fields.", nil)
		return
	}
	limit := int64(voices.MaxUploadBytes + VoiceMultipartOverhead)
	tooLarge := func() {
		w.Header().Set("Connection", "close")
		httpx.Error(w, r, 413, "invalid_request_error", "audio_too_large",
			fmt.Sprintf("Audio exceeds the %d MiB limit.", voices.MaxUploadBytes/(1024*1024)), httpx.StrPtr("audio"))
	}
	if r.ContentLength > limit {
		tooLarge()
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		httpx.BadRequest(w, r, "Invalid form data payload.", nil)
		return
	}
	if int64(len(body)) > limit {
		tooLarge()
		return
	}
	form, err := httpx.ReadMultipart(r, body)
	if err != nil {
		httpx.BadRequest(w, r, "Invalid form data payload.", nil)
		return
	}
	first := func(field string) *httpx.FormPart {
		if v := form[field]; len(v) > 0 {
			return &v[0]
		}
		return nil
	}
	n, c, a := first("name"), first("consent"), first("audio")
	if n == nil || n.IsFile {
		httpx.Error(w, r, 400, "invalid_request_error", "invalid_voice_name", "Voice name is required.", httpx.StrPtr("name"))
		return
	}
	if a == nil || !a.IsFile {
		httpx.Error(w, r, 400, "invalid_request_error", "unsupported_audio_format", "Attach the reference recording as the audio field.", httpx.StrPtr("audio"))
		return
	}
	// The upload's filename and declared type are ignored; only its bytes count.
	return n.Value, c != nil && !c.IsFile && c.Value == "true", a.File, true
}

// jsonKindOf names a decoded JSON value's type as zod reports it.
func jsonKindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	}
	return "object"
}

func parseVoiceRename(w http.ResponseWriter, r *http.Request) (string, bool) {
	const limit = 1024
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if r.ContentLength > limit || len(b) > limit {
		w.Header().Set("Connection", "close")
		httpx.Error(w, r, 413, "invalid_request_error", "validation_failed", "Voice update body exceeds the 1024 byte limit.", nil)
		return "", false
	}
	var input any
	if err != nil || json.Unmarshal(b, &input) != nil {
		httpx.BadRequest(w, r, "Send a JSON body with a name.", nil)
		return "", false
	}
	invalid := func(msg string) (string, bool) {
		httpx.Error(w, r, 400, "invalid_request_error", "invalid_voice_name", msg, httpx.StrPtr("name"))
		return "", false
	}
	obj, ok := input.(map[string]any)
	if !ok {
		return invalid("Invalid input: expected object, received " + jsonKindOf(input))
	}
	raw, present := obj["name"]
	name, isString := raw.(string)
	switch {
	case !present:
		return invalid("Invalid input: expected string, received undefined")
	case !isString:
		return invalid("Invalid input: expected string, received " + jsonKindOf(raw))
	}
	if _, err := voices.ParseName(name); err != nil {
		return invalid(err.Error())
	}
	var extra []string
	for _, k := range objectKeys(b) {
		if k != "name" {
			extra = append(extra, strconv.Quote(k))
		}
	}
	switch len(extra) {
	case 0:
		return name, true
	case 1:
		return invalid("Unrecognized key: " + extra[0])
	}
	return invalid("Unrecognized keys: " + strings.Join(extra, ", "))
}

// objectKeys lists a JSON object's top-level keys in input order, once each.
func objectKeys(b []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	var keys []string
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return keys
		}
		k, _ := t.(string)
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return keys
		}
	}
	return keys
}
