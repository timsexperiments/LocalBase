package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/video"
)

func decodeNumbers(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func message(c *checker) any {
	if c.ok() {
		return nil
	}
	parts := make([]string, len(c.issues))
	for i, is := range c.issues {
		parts[i] = is.path + ": " + is.msg
	}
	return "Validation failed: " + strings.Join(parts, ", ")
}

// canonical re-encodes a value so Go and TS JSON compare structurally.
func canonical(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	json.Unmarshal(b, &out)
	return out
}

// TestVideoGolden pins the /v1/videos contract to TS
// (tools/golden/video.ts): create-request validation messages, native input
// validation, qualification and mismatch messages for every catalog video
// model, and the job projections with their failure codes and messages.
func TestVideoGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/video.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Requests map[string]struct {
			Body    json.RawMessage `json:"body"`
			Message any             `json:"message"`
		} `json:"requests"`
		Inputs struct {
			Cases map[string]json.RawMessage `json:"cases"`
			Valid map[string]bool            `json:"valid"`
		} `json:"inputs"`
		Models []struct {
			ModelID string `json:"modelId"`
			Cases   []struct {
				Name      string          `json:"name"`
				Body      json.RawMessage `json:"body"`
				Schema    any             `json:"schema"`
				Qualified any             `json:"qualified"`
				Mismatch  any             `json:"mismatch"`
			} `json:"cases"`
		} `json:"models"`
		Projections map[string]any `json:"projections"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	t.Run("requests", func(t *testing.T) {
		for name, c := range g.Requests {
			if got := message(validateVideoCreate(decodeNumbers(t, c.Body))); !reflect.DeepEqual(got, c.Message) {
				t.Errorf("%s: %v\nwant %v", name, got, c.Message)
			}
		}
	})
	t.Run("inputs", func(t *testing.T) {
		for name, body := range g.Inputs.Cases {
			in, _ := decodeNumbers(t, body).(map[string]any)
			if got := validVideoInput(in); got != g.Inputs.Valid[name] {
				t.Errorf("%s: valid %v, want %v", name, got, g.Inputs.Valid[name])
			}
		}
	})
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("models", func(t *testing.T) {
		if len(g.Models) == 0 {
			t.Fatal("no models")
		}
		for _, m := range g.Models {
			spec := cat.ByID(m.ModelID)
			for _, c := range m.Cases {
				body := decodeNumbers(t, c.Body)
				if c.Schema != nil {
					if got := message(validateVideoCreate(body)); !reflect.DeepEqual(got, c.Schema) {
						t.Errorf("%s %s: %v, want %v", m.ModelID, c.Name, got, c.Schema)
					}
					continue
				}
				if v := validateVideoCreate(body); !v.ok() {
					t.Errorf("%s %s: refused %v", m.ModelID, c.Name, message(v))
					continue
				}
				req := body.(map[string]any)
				q := qualifiedVideoInput(req, spec)
				var got any
				if q != nil {
					got = canonical(t, q)
				}
				if !reflect.DeepEqual(got, c.Qualified) {
					t.Errorf("%s %s: qualified %v\nwant %v", m.ModelID, c.Name, got, c.Qualified)
				}
				var mismatch any
				if q == nil {
					mismatch = videoProfileMismatchMessage(req, spec)
				}
				if !reflect.DeepEqual(mismatch, c.Mismatch) {
					t.Errorf("%s %s: mismatch %v\nwant %v", m.ModelID, c.Name, mismatch, c.Mismatch)
				}
			}
		}
	})
	t.Run("projections", func(t *testing.T) {
		created, terminal := time.UnixMilli(1_760_000_000_123), time.UnixMilli(1_760_000_042_999)
		id := "6f1c2c3e-8a7b-4c1d-9e2f-0123456789ab"
		gib := func(f float64) int64 { return int64(f * float64(memory.GiB)) }
		diag := func(requested, effective, reserve int64) *memory.AdmissionDiagnostics {
			return &memory.AdmissionDiagnostics{Requested: requested, EffectiveAvailable: effective, Reserve: reserve,
				MeasuredAvailable: -1, Pending: -1}
		}
		failures := map[string]error{
			"generic":               errors.New("boom"),
			"pressure":              &memory.AdmissionError{Reason: "memory-pressure", PoolID: "system"},
			"accelerator-pressure":  &memory.AdmissionError{Reason: "memory-pressure", PoolID: "system"},
			"measurement":           &memory.AdmissionError{Reason: "measurement-unavailable", PoolID: "system"},
			"system-memory":         &memory.AdmissionError{Reason: "system-memory", PoolID: "system", Diagnostics: diag(gib(12), gib(15), gib(9.6))},
			"system-memory-unknown": &memory.AdmissionError{Reason: "system-memory", PoolID: "system", Diagnostics: diag(-1, -1, gib(9.6))},
			"accelerator-memory":    &memory.AdmissionError{Reason: "accelerator-memory", PoolID: "system", Diagnostics: diag(gib(30), gib(20), gib(2))},
			"gpu-working-set": &memory.AdmissionError{Reason: "accelerator-memory", PoolID: "gpu-working-set",
				RequestedBytes: gib(50), AvailableBytes: gib(48), Diagnostics: diag(gib(20), -1, -1)},
			"consent": &runtime.NotInstalledError{ModelID: "wan2.1-t2v-1.3b-q8_0"},
		}
		jobs := map[string]video.Job{
			"queued":      {ID: id, State: video.Queued, CreatedAt: created},
			"in_progress": {ID: id, State: video.InProgress, CreatedAt: created},
			"completed": {ID: id, State: video.Completed, CreatedAt: created, TerminalAt: terminal,
				Artifact: video.Artifact{Bytes: 123_456, MimeType: "video/mp4", Format: "mp4", FPS: 16, Frames: 33}},
		}
		for _, r := range []string{"backend", "cancelled", "deadline", "shutdown"} {
			jobs["cancelled-"+r] = video.Job{ID: id, State: video.Cancelled, CreatedAt: created, TerminalAt: terminal, Reason: r}
		}
		for name, f := range failures {
			jobs["failed-"+name] = video.Job{ID: id, State: video.Failed, CreatedAt: created, TerminalAt: terminal, Failure: f}
		}
		if len(jobs) != len(g.Projections) {
			t.Fatalf("%d projections, golden has %d", len(jobs), len(g.Projections))
		}
		for name, j := range jobs {
			if got := canonical(t, projectVideoJob(j)); !reflect.DeepEqual(got, g.Projections[name]) {
				t.Errorf("%s: %v\nwant %v", name, got, g.Projections[name])
			}
		}
	})
}
