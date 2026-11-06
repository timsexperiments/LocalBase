package video

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
)

// The stable-diffusion.cpp video API (TS stable-diffusion-video-client.ts):
// /sdcpp/v1/vid_gen and /sdcpp/v1/jobs/{id} on a numeric loopback address,
// with bounded responses and checked container signatures.

const (
	defaultMaxResponseBytes = 32 << 20
	defaultMaxMediaBytes    = 23 << 20
)

var jobIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ClientError is a failed backend exchange.
type ClientError struct {
	Code    string // invalid_base_url | invalid_job_id | backend_response_too_large | backend_media_too_large | backend_response_invalid | backend_request_failed
	Message string
	Status  int
}

func (e *ClientError) Error() string { return e.Message }

func invalidResponse() error {
	return &ClientError{Code: "backend_response_invalid", Message: "Video backend returned an invalid response."}
}

// Client talks to one backend.
type Client struct {
	base        *url.URL
	http        *http.Client
	maxResponse int64
	maxMedia    int64
}

// NewClient accepts only http://127.0.0.1:<port>/ or http://[::1]:<port>/,
// so a DNS change cannot redirect video traffic.
func NewClient(baseURL string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || (u.Path != "/" && u.Path != "") {
		return nil, &ClientError{Code: "invalid_base_url", Message: "Video backend URL must use a numeric loopback HTTP URL without a path."}
	}
	return &Client{base: u, maxResponse: defaultMaxResponseBytes, maxMedia: defaultMaxMediaBytes,
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}}, nil
}

type envelope struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Status  string          `json:"status"`
	Result  json.RawMessage `json:"result"`
	Created *float64        `json:"created"`
}

// Submit posts the native input (toBackendVideoInput).
func (c *Client) Submit(ctx context.Context, input map[string]any) (BackendJob, error) {
	body, _ := json.Marshal(backendInput(input))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/sdcpp/v1/vid_gen"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	env, err := c.job(req)
	if err != nil {
		return BackendJob{}, err
	}
	if env.Status != "queued" && env.Status != "generating" {
		return BackendJob{}, invalidResponse()
	}
	return BackendJob{ID: env.ID, Status: env.Status}, nil
}

// Get reads a job, decoding its media when completed.
func (c *Client) Get(ctx context.Context, id string) (BackendJob, error) {
	if !jobIDPattern.MatchString(id) {
		return BackendJob{}, &ClientError{Code: "invalid_job_id", Message: "Video job ID is invalid."}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/sdcpp/v1/jobs/"+url.PathEscape(id)), nil)
	env, err := c.job(req)
	if err != nil {
		return BackendJob{}, err
	}
	if env.ID != id {
		return BackendJob{}, invalidResponse()
	}
	switch env.Status {
	case "queued", "generating", "failed", "cancelled":
		return BackendJob{ID: env.ID, Status: env.Status}, nil
	case "completed":
		media, err := c.media(env.Result)
		if err != nil {
			return BackendJob{}, err
		}
		return BackendJob{ID: env.ID, Status: "completed", Media: media}, nil
	}
	return BackendJob{}, invalidResponse()
}

func (c *Client) endpoint(path string) string {
	u := *c.base
	u.Path = path
	return u.String()
}

func (c *Client) job(req *http.Request) (envelope, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return envelope{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return envelope{}, &ClientError{Code: "backend_request_failed", Status: resp.StatusCode,
			Message: fmt.Sprintf("Video backend request failed with HTTP %d.", resp.StatusCode)}
	}
	if resp.ContentLength > c.maxResponse {
		return envelope{}, &ClientError{Code: "backend_response_too_large", Message: "Video backend response exceeds the configured limit."}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponse+1))
	if err != nil {
		return envelope{}, err
	}
	if int64(len(raw)) > c.maxResponse {
		return envelope{}, &ClientError{Code: "backend_response_too_large", Message: "Video backend response exceeds the configured limit."}
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil || !jobIDPattern.MatchString(env.ID) || env.Kind != "vid_gen" || env.Created == nil || *env.Created < 0 || *env.Created != float64(int64(*env.Created)) {
		return envelope{}, invalidResponse()
	}
	switch env.Status {
	case "queued", "generating", "completed", "failed", "cancelled":
		return env, nil
	}
	return envelope{}, invalidResponse()
}

func (c *Client) media(raw json.RawMessage) (*Media, error) {
	var r map[string]any
	if json.Unmarshal(raw, &r) != nil || r == nil {
		return nil, invalidResponse()
	}
	for k := range r {
		switch k {
		case "fps", "frame_count", "b64_json", "output_format", "mime_type":
		default:
			return nil, invalidResponse()
		}
	}
	format, _ := r["output_format"].(string)
	mime, _ := r["mime_type"].(string)
	want := map[string]string{"webm": "video/webm", "webp": "image/webp", "avi": "video/x-msvideo"}[format]
	fps, okFPS := r["fps"].(float64)
	frames, okFrames := r["frame_count"].(float64)
	b64, _ := r["b64_json"].(string)
	if want == "" || mime != want || !okFPS || fps <= 0 || fps != float64(int(fps)) || !okFrames || frames <= 0 ||
		frames != float64(int(frames)) || b64 == "" {
		return nil, invalidResponse()
	}
	if int64(len(b64)) > (c.maxMedia+2)/3*4 {
		return nil, &ClientError{Code: "backend_media_too_large", Message: "Video backend media exceeds the configured limit."}
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, invalidResponse()
	}
	if int64(len(data)) > c.maxMedia {
		return nil, &ClientError{Code: "backend_media_too_large", Message: "Video backend media exceeds the configured limit."}
	}
	if !containerSignature(format, data) {
		return nil, invalidResponse()
	}
	return &Media{Data: data, MimeType: mime, Format: format, FPS: int(fps), Frames: int(frames)}, nil
}

func containerSignature(format string, b []byte) bool {
	switch format {
	case "webm":
		return bytes.HasPrefix(b, []byte{0x1a, 0x45, 0xdf, 0xa3})
	case "webp":
		return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP"
	case "avi":
		return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "AVI "
	}
	return false
}

// backendInput is the sd-server request body (TS toBackendVideoInput).
func backendInput(in map[string]any) map[string]any {
	out := map[string]any{"prompt": in["prompt"]}
	for from, to := range map[string]string{"negativePrompt": "negative_prompt", "width": "width", "height": "height",
		"videoFrames": "video_frames", "fps": "fps", "seed": "seed", "outputFormat": "output_format"} {
		if v, ok := in[from]; ok {
			out[to] = v
		}
	}
	if g, ok := in["generation"].(map[string]any); ok {
		out["sample_params"] = map[string]any{"sample_method": g["sampler"], "scheduler": g["scheduler"],
			"sample_steps": g["steps"], "flow_shift": g["flowShift"], "guidance": map[string]any{"txt_cfg": g["cfgScale"]}}
	}
	switch in["kind"] {
	case "speech":
		portrait, _ := in["portrait"].(map[string]any)
		audio, _ := in["audio"].(map[string]any)
		out["init_image"] = portrait["data"]
		out["audio"] = map[string]any{"format": "wav", "data": audio["data"]}
	case "image":
		image, _ := in["image"].(map[string]any)
		out["init_image"] = image["data"]
	}
	return out
}
