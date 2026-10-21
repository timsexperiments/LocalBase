package gateway

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/video"
)

// The /v1/videos contract (TS video/gateway-contract.ts): the create request,
// its qualification against the model's catalog profile, and the job
// projection.

// validateVideoCreate checks a create request as videoCreateRequestSchema
// does, in its field order, then its refinement.
func validateVideoCreate(v any) *checker {
	c := &checker{}
	obj, ok := v.(map[string]any)
	if !ok {
		c.fail("", expected("object", v, true))
		return c
	}
	c.videoString(obj, "model", 1, 0, true)
	c.videoString(obj, "prompt", 1, 16_384, true)
	c.videoString(obj, "negative_prompt", 0, 16_384, false)
	for _, k := range []string{"width", "height", "frames", "fps"} {
		c.positiveInt(obj, k)
	}
	c.conditioning(obj)
	c.videoString(obj, "input_reference", 0, 6_000_000, false)
	c.strict(obj, "", "model", "prompt", "negative_prompt", "width", "height", "frames", "fps", "input", "input_reference")
	if c.ok() {
		if _, ref := obj["input_reference"]; ref {
			if in, _ := obj["input"].(map[string]any); in["kind"] != "text" {
				c.fail("input_reference", "input_reference can only be combined with input.kind=text.")
			}
		}
	}
	return c
}

func (c *checker) videoString(obj map[string]any, key string, lo, hi int, required bool) {
	c.videoStringAt(obj, key, key, lo, hi, required)
}

func (c *checker) videoStringAt(obj map[string]any, key, path string, lo, hi int, required bool) {
	v, present := obj[key]
	if !present {
		if required {
			c.fail(path, expected("string", nil, false))
		}
		return
	}
	s, ok := v.(string)
	if !ok {
		c.fail(path, expected("string", v, true))
		return
	}
	n := utf16Length(s)
	if n < lo {
		c.fail(path, fmt.Sprintf("Too small: expected string to have >=%d characters", lo))
	}
	if hi > 0 && n > hi {
		c.fail(path, fmt.Sprintf("Too big: expected string to have <=%d characters", hi))
	}
}

func (c *checker) positiveInt(obj map[string]any, key string) {
	v, present := obj[key]
	if !present {
		c.fail(key, expected("number", nil, false))
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
	if _, isInt := jsInt(v); !isInt {
		c.fail(key, "Invalid input: expected int, received number")
		return
	}
	if f <= 0 {
		c.fail(key, "Too small: expected number to be >0")
	}
}

// conditioning checks input (videoConditioningInputSchema).
func (c *checker) conditioning(obj map[string]any) {
	v, present := obj["input"]
	in, ok := v.(map[string]any)
	if !ok {
		c.fail("input", expected("object", v, present))
		return
	}
	switch in["kind"] {
	case "text":
		c.strict(in, "input", "kind")
	case "speech":
		c.inlineMedia(in, "input.portrait", "portrait", "png", maxBase64PNGLength)
		c.inlineMedia(in, "input.audio", "audio", "wav", maxBase64WAVLength)
		c.strict(in, "input", "kind", "portrait", "audio")
	case "image":
		c.inlineMedia(in, "input.image", "image", "png", maxBase64PNGLength)
		c.strict(in, "input", "kind", "image")
	default:
		c.fail("input.kind", "Invalid discriminator value. Expected 'text' | 'speech' | 'image'")
	}
}

func (c *checker) inlineMedia(parent map[string]any, path, key, format string, maxLength int) {
	v, present := parent[key]
	m, ok := v.(map[string]any)
	if !ok {
		c.fail(path, expected("object", v, present))
		return
	}
	if m["format"] != format {
		c.fail(path+".format", `Invalid input: expected "`+format+`"`)
	}
	c.videoStringAt(m, "data", path+".data", 1, maxLength, true)
	c.strict(m, path, "format", "data")
}

// qualifiedVideoInput maps a valid request to the native input when it
// matches the model's qualified profile exactly (TS qualifiedVideoInput).
func qualifiedVideoInput(req map[string]any, spec *catalog.Spec) map[string]any {
	if spec == nil || spec.Kind != "video" || spec.VideoRuntime == nil {
		return nil
	}
	rt := spec.VideoRuntime
	q := rt.Qualification
	if num(req["width"]) != float64(q.MaxWidth) || num(req["height"]) != float64(q.MaxHeight) ||
		num(req["frames"]) != float64(q.MaxFrames) || num(req["fps"]) != float64(q.FPS) {
		return nil
	}
	input, _ := req["input"].(map[string]any)
	kind, _ := input["kind"].(string)
	_, hasRef := req["input_reference"]
	if (hasRef && rt.Mode != "ti2v") || (rt.Mode == "t2v" && kind != "text") || (rt.Mode == "s2v" && kind != "speech") ||
		(rt.Mode == "ti2v" && kind != "text" && kind != "image") {
		return nil
	}
	g := q.Generation
	candidate := map[string]any{"kind": kind, "prompt": req["prompt"], "width": q.MaxWidth, "height": q.MaxHeight,
		"videoFrames": q.MaxFrames, "fps": q.FPS, "outputFormat": "avi",
		"generation": map[string]any{"sampler": g.Sampler, "scheduler": g.Scheduler, "steps": g.Steps,
			"cfgScale": g.CfgScale, "flowShift": g.FlowShift}}
	if g.Seed != nil {
		candidate["seed"] = *g.Seed
	}
	if np, ok := req["negative_prompt"]; ok {
		candidate["negativePrompt"] = np
	}
	switch {
	case hasRef:
		candidate["kind"] = "image"
		ref, _ := req["input_reference"].(string)
		candidate["image"] = map[string]any{"format": "png", "data": imageReferenceData(ref)}
	case kind == "speech":
		candidate["portrait"], candidate["audio"] = input["portrait"], input["audio"]
	case kind == "image":
		candidate["image"] = input["image"]
	}
	if !validVideoInput(jsonNumbers(candidate)) {
		return nil
	}
	return candidate
}

// jsonNumbers gives ints the json.Number form validation reads.
func jsonNumbers(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		switch n := v.(type) {
		case int:
			out[k] = float64(n)
		case int64:
			out[k] = float64(n)
		case map[string]any:
			out[k] = jsonNumbers(n)
		default:
			out[k] = v
		}
	}
	return out
}

func imageReferenceData(ref string) string {
	const prefix = "data:image/png;base64,"
	if strings.HasPrefix(ref, prefix) && canonicalDataURLPayload(ref[len(prefix):]) {
		return ref[len(prefix):]
	}
	return ref
}

// canonicalDataURLPayload is the TS /^data:image\/png;base64,([A-Za-z0-9+/]+=*)$/ match.
func canonicalDataURLPayload(s string) bool {
	i := 0
	for i < len(s) && (s[i] >= 'A' && s[i] <= 'Z' || s[i] >= 'a' && s[i] <= 'z' || s[i] >= '0' && s[i] <= '9' || s[i] == '+' || s[i] == '/') {
		i++
	}
	if i == 0 {
		return false
	}
	for i < len(s) && s[i] == '=' {
		i++
	}
	return i == len(s)
}

// videoProfileMismatchMessage names the accepted profile (TS).
func videoProfileMismatchMessage(req map[string]any, spec *catalog.Spec) string {
	if spec == nil || spec.Kind != "video" || spec.VideoRuntime == nil {
		return "This model does not support video generation."
	}
	q, mode := spec.VideoRuntime.Qualification, spec.VideoRuntime.Mode
	input, _ := req["input"].(map[string]any)
	kind, _ := input["kind"].(string)
	inputKind := "text"
	if mode == "s2v" {
		inputKind = "speech"
	}
	wrongInput := kind != inputKind
	if mode == "ti2v" {
		wrongInput = kind != "text" && kind != "image"
	}
	got := fmt.Sprintf("%vx%v", trimFloat(num(req["width"])), trimFloat(num(req["height"])))
	if ref, ok := req["input_reference"].(string); ok && ref != "" {
		encoded := strings.TrimPrefix(ref, "data:image/png;base64,")
		if png, err := base64.StdEncoding.DecodeString(encoded); err == nil && len(png) >= 24 {
			got = fmt.Sprintf("image %dx%d", binary.BigEndian.Uint32(png[16:]), binary.BigEndian.Uint32(png[20:]))
		}
	}
	msg := fmt.Sprintf("This local video model accepts exactly %dx%d (width x height), %d frames at %d fps; got %s, %s frames at %s fps.",
		q.MaxWidth, q.MaxHeight, q.MaxFrames, q.FPS, got, trimFloat(num(req["frames"])), trimFloat(num(req["fps"])))
	if wrongInput {
		if mode == "ti2v" {
			msg += " It accepts text input or a PNG input_reference."
		} else {
			msg += ` It also requires input.kind "` + inputKind + `".`
		}
	}
	return msg
}

// projectVideoJob is the public job object (TS projectVideoJob).
func projectVideoJob(j video.Job) map[string]any {
	out := map[string]any{"object": "localbase.video.job", "id": j.ID, "status": j.State, "created_at": j.CreatedAt.Unix()}
	if j.State == video.Queued || j.State == video.InProgress {
		return out
	}
	out["completed_at"] = j.TerminalAt.Unix()
	switch j.State {
	case video.Completed:
		a := j.Artifact
		out["content_type"], out["bytes"], out["fps"], out["frames"] = a.MimeType, a.Bytes, a.FPS, a.Frames
	case video.Failed:
		out["error"] = videoFailureError(j.Failure)
	case video.Cancelled:
		out["cancellation_reason"] = j.Reason
	}
	return out
}

const freeMemoryHint = "Free memory (unload other models or close applications) and retry."

// videoFailureError names a failed job's error code (TS videoFailureError).
func videoFailureError(err error) map[string]any {
	var ae *memory.AdmissionError
	var consent *runtime.NotInstalledError
	var launch *runtime.LaunchError
	switch {
	case errors.As(err, &ae):
		return map[string]any{"code": "insufficient_memory", "message": videoMemoryMessage(ae)}
	case errors.As(err, &consent):
		return map[string]any{"code": "model_install_consent_required", "message": consent.Error()}
	case errors.As(err, &launch) && launch.Code == "model_hardware_ineligible":
		return map[string]any{"code": "model_hardware_ineligible", "message": launch.Message}
	case errors.As(err, &launch) && launch.Code == "experimental_model_not_allowed":
		return map[string]any{"code": "experimental_model_not_allowed", "message": launch.Message}
	}
	return map[string]any{"code": "video_generation_failed"}
}

func gibText(v int64) string {
	if v < 0 {
		return "an unknown amount"
	}
	return fmt.Sprintf("%.1f GiB", float64(v)/float64(memory.GiB))
}

func videoMemoryMessage(e *memory.AdmissionError) string {
	accelerator := e.Reason == "accelerator-memory"
	switch {
	case e.Reason == "memory-pressure":
		prefix := ""
		if accelerator {
			prefix = "accelerator "
		}
		return "The host is under " + prefix + "memory pressure, so the video runtime was not started. " + freeMemoryHint
	case e.Reason == "measurement-unavailable":
		return "Available memory could not be measured reliably, so the video runtime was not started. Retry shortly."
	case e.PoolID == "gpu-working-set" && !e.Permanent:
		required, resident := e.RequestedBytes, e.RequestedBytes
		if e.Diagnostics != nil {
			required = e.Diagnostics.Requested
		}
		resident = e.RequestedBytes - required
		return fmt.Sprintf("Not enough GPU working set to start the video runtime alongside the models already loaded: it needs %s, other loaded models use %s, and the GPU working-set limit is %s. Unload other models and retry.",
			gibText(required), gibText(resident), gibText(e.AvailableBytes))
	}
	pool := "memory"
	if accelerator {
		pool = "accelerator (GPU) memory"
	}
	requested, usable := int64(-1), int64(-1)
	if g := e.Diagnostics; g != nil {
		requested = g.Requested
		if g.EffectiveAvailable >= 0 && g.Reserve >= 0 {
			usable = max(0, g.EffectiveAvailable-g.Reserve)
		}
	}
	return fmt.Sprintf("Not enough free %s to start the video runtime: it needs %s but only %s is usable after the safety reserve. %s",
		pool, gibText(requested), gibText(usable), freeMemoryHint)
}
