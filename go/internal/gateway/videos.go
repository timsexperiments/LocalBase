package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/video"
)

// /v1/videos (TS video/gateway-handler.ts and the serve wiring): create a
// job, read its status, fetch its artifact, cancel or delete it. Jobs belong
// to the principal that created them; without auth they belong to the local
// owner.

const (
	localVideoOwner     = "anonymous:local"
	videoMultipartLimit = 9 << 20
	videoReferenceLimit = 4 << 20
	videoJobIDPattern   = `^[0-9a-fA-F-]{36}$`
)

var videoJobID = regexp.MustCompile(videoJobIDPattern)

// videoOwner names who owns the request's jobs (TS principalOwnerId).
func (s *Server) videoOwner(r *http.Request) string {
	p := PrincipalFrom(r.Context())
	if s.NoAuth || p == nil || p.Kind == "anonymous" {
		return localVideoOwner
	}
	return p.OwnerID()
}

// videoJobRoute serves /v1/videos/{job_id}[/content|/cancel]; any other
// path under /v1/videos/ is an unmatched route, as in TS.
func (s *Server) videoJobRoute(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("job_id")
		if !videoJobID.MatchString(id) {
			s.unmatched(w, r)
			return
		}
		s.guard("inference:video", func(w http.ResponseWriter, r *http.Request) { s.videoJob(w, r, action, id) })(w, r)
	}
}

func (s *Server) videoJob(w http.ResponseWriter, r *http.Request, action, id string) {
	owner := s.videoOwner(r)
	switch action {
	case "status":
		if r.Method == http.MethodDelete {
			if s.VideoJobs == nil || !s.VideoJobs.Delete(owner, id) {
				videoJobNotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet {
			methodNotAllowed(w, r, "GET, DELETE")
			return
		}
		if s.VideoJobs == nil {
			videoJobNotFound(w, r)
			return
		}
		j, ok := s.VideoJobs.Get(owner, id)
		if !ok {
			videoJobNotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, projectVideoJob(j))
	case "content":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, r, "GET")
			return
		}
		if s.VideoJobs == nil {
			videoJobNotFound(w, r)
			return
		}
		path, a, ok := s.VideoJobs.Artifact(owner, id)
		if !ok {
			videoJobNotFound(w, r)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			videoJobNotFound(w, r)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", a.MimeType)
		w.Header().Set("Content-Length", strconv.FormatInt(a.Bytes, 10))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "private, no-store")
		w.WriteHeader(http.StatusOK)
		io.Copy(w, f)
	case "cancel":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, r, "POST")
			return
		}
		if s.VideoJobs == nil {
			videoJobNotFound(w, r)
			return
		}
		j, ok, _ := s.VideoJobs.Cancel(owner, id)
		if !ok {
			videoJobNotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, projectVideoJob(j))
	}
}

func videoJobNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, 404, "invalid_request_error", "video_job_not_found", "Video job not found.", nil)
}

// videoCreate starts a job (TS handleVideoGatewayRequest, videoCreate).
func (s *Server) videoCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, "POST")
		return
	}
	if r.Context().Err() != nil {
		w.WriteHeader(499)
		return
	}
	if s.Video == nil || s.VideoJobs == nil || len(s.Video.Config().Selected["video"]) == 0 {
		notConfigured(w, r, "Video")
		return
	}
	req, ok := s.readVideoCreate(w, r)
	if !ok {
		return
	}
	if c := validateVideoCreate(req); !c.ok() {
		c.reply(w, r)
		return
	}
	model, _ := req["model"].(string)
	spec := s.Catalog.ByID(model)
	if spec == nil || spec.Kind != "video" || spec.VideoRuntime == nil {
		modelNotFound(w, r, model)
		return
	}
	input := qualifiedVideoInput(req, spec)
	if input == nil {
		badRequest(w, r, videoProfileMismatchMessage(req, spec), nil)
		return
	}
	if err := s.Video.InstallConsent(model); err != nil {
		writeError(w, r, 409, "invalid_request_error", "model_install_consent_required", err.Error(), strp("model"))
		return
	}
	requestID := w.Header().Get("X-Localbase-Request-Id")
	var lease *runtime.ServiceLease
	acquire := func(ctx context.Context) (video.Admission, error) {
		l, err := s.Video.Acquire(ctx, model)
		if err != nil {
			return nil, err
		}
		lease = l
		return leaseAdmission{l}, nil
	}
	stop := func() error {
		if lease != nil {
			return lease.Kill()
		}
		return nil
	}
	deadline := time.Duration(spec.VideoRuntime.Qualification.JobDeadlineMs) * time.Millisecond
	job, terminal, err := s.VideoJobs.Start(s.videoOwner(r), input, deadline, acquire, stop)
	if errors.Is(err, video.ErrBusy) {
		w.Header().Set("Retry-After", "1")
		writeError(w, r, 429, "rate_limit_error", "video_job_busy", "A local video job is already in progress.", nil)
		return
	}
	if err != nil {
		serviceUnavailable(w, r, "Video")
		return
	}
	go func() {
		j := <-terminal
		level, attrs := "info", []any{"event", "video.job-terminal", "category", "runtime", "component", "video-job-manager",
			"runtime", "video", "request_id", requestID}
		if j.State == video.Failed {
			level = "error"
			attrs = append(attrs, "error_type", "VideoJobFailure", "error", "A local video job failed.")
		}
		duration := int64(0)
		if !j.TerminalAt.IsZero() {
			duration = j.TerminalAt.Sub(j.CreatedAt).Milliseconds()
		}
		attrs = append(attrs, slog.Group("attributes", "job_id", j.ID, "model_id", model, "state", string(j.State), "duration_ms", duration))
		if level == "error" {
			s.Log.Error("A local video job reached a terminal state.", attrs...)
		} else {
			s.Log.Info("A local video job reached a terminal state.", attrs...)
		}
	}()
	writeJSON(w, http.StatusAccepted, projectVideoJob(job))
}

// leaseAdmission holds the video runtime for a job: Acquire already waited
// for it to start.
type leaseAdmission struct{ l *runtime.ServiceLease }

func (a leaseAdmission) Ready(context.Context) error { return nil }
func (a leaseAdmission) Release()                    { a.l.Release() }

// readVideoCreate reads a JSON body, or a multipart form with an optional
// PNG input_reference file (TS parseCreateRequest).
func (s *Server) readVideoCreate(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data;") {
		return readJSON(w, r)
	}
	if r.ContentLength > videoMultipartLimit {
		payloadTooLargeLimit(w, r, videoMultipartLimit)
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, videoMultipartLimit+1))
	if err == nil && len(body) > videoMultipartLimit {
		payloadTooLargeLimit(w, r, videoMultipartLimit)
		return nil, false
	}
	var form map[string][]formPart
	if err == nil {
		form, err = readMultipart(r, body)
	}
	if err != nil {
		badRequest(w, r, "Invalid multipart video request.", nil)
		return nil, false
	}
	get := func(name string) (formPart, bool) {
		if parts := form[name]; len(parts) > 0 {
			return parts[0], true
		}
		return formPart{}, false
	}
	text := func(name string) any {
		p, ok := get(name)
		switch {
		case !ok:
			return nil
		case p.isFile:
			return map[string]any{}
		}
		return p.value
	}
	numberOf := func(name string) any {
		p, ok := get(name)
		switch {
		case !ok:
			return float64(0) // Number(null)
		case p.isFile:
			return math.NaN()
		}
		return jsNumber(p.value)
	}
	payload := map[string]any{"model": text("model"), "prompt": text("prompt"), "width": numberOf("width"),
		"height": numberOf("height"), "frames": numberOf("frames"), "fps": numberOf("fps"), "input": map[string]any{"kind": "text"}}
	if _, ok := get("negative_prompt"); ok {
		payload["negative_prompt"] = text("negative_prompt")
	}
	if p, ok := get("input_reference"); ok {
		if p.isFile {
			if len(p.file) > videoReferenceLimit || imageMimeFromMagic(p.file) != "image/png" {
				badRequest(w, r, "input_reference must be a PNG file no larger than 4 MiB.", strp("input_reference"))
				return nil, false
			}
			payload["input_reference"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(p.file)
		} else {
			payload["input_reference"] = p.value
		}
	}
	if p, ok := get("input"); ok && !p.isFile {
		dec := json.NewDecoder(bytes.NewReader([]byte(p.value)))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) != nil || dec.More() {
			badRequest(w, r, "input must be valid JSON.", strp("input"))
			return nil, false
		}
		payload["input"] = v
	}
	if c := validateVideoCreate(payload); !c.ok() {
		badRequest(w, r, "Invalid video request.", nil)
		return nil, false
	}
	return payload, true
}
