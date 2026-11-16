package voices

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

type spec struct {
	Case       string `json:"case"`
	Tag        *int   `json:"tag"`
	Channels   *int   `json:"channels"`
	Rate       *int   `json:"rate"`
	Bits       *int   `json:"bits"`
	Frames     *int   `json:"frames"`
	Seed       *uint32
	Amp        *float64 `json:"amp"`
	Extensible bool     `json:"extensible"`
	BlockAlign *int     `json:"blockAlign"`
	Silent     bool     `json:"silent"`
	NaNAt      *int     `json:"nanAt"`
	NoFmt      bool     `json:"noFmt"`
	NoData     bool     `json:"noData"`
	DupFmt     bool     `json:"dupFmt"`
	DupData    bool     `json:"dupData"`
	PreChunks  []struct {
		ID   string `json:"id"`
		Size int    `json:"size"`
	} `json:"preChunks"`
	Truncate int     `json:"truncate"`
	Raw      *string `json:"raw"`
}

func or[T any](p *T, d T) T {
	if p != nil {
		return *p
	}
	return d
}

// build mirrors tools/golden/voices.ts build().
func build(s spec) []byte {
	if s.Raw != nil {
		b, _ := base64.StdEncoding.DecodeString(*s.Raw)
		return b
	}
	x := or(s.Seed, 1)
	next := func() uint32 { x = x*1664525 + 1013904223; return x }
	tag, channels, rate, bits := or(s.Tag, 1), or(s.Channels, 1), or(s.Rate, 16000), or(s.Bits, 16)
	frames := or(s.Frames, rate*4)
	// TS: bytesPerSample = bits / 8 may be fractional; the size field
	// truncates it and the byte loop rounds it up.
	written := (bits + 7) / 8
	blockAlign := or(s.BlockAlign, channels*bits/8)
	var b []byte
	u8 := func(v int) { b = append(b, byte(v)) }
	u16 := func(v int) { b = binary.LittleEndian.AppendUint16(b, uint16(v)) }
	u32 := func(v int) { b = binary.LittleEndian.AppendUint32(b, uint32(v)) }
	ascii := func(t string) { b = append(b, t...) }
	fmtChunk := func() {
		ascii("fmt ")
		if s.Extensible {
			u32(40)
			u16(0xfffe)
		} else {
			u32(16)
			u16(tag)
		}
		u16(channels)
		u32(rate)
		// TS: rate * blockAlign, where an unset blockAlign is fractional for 12-bit.
		if s.BlockAlign == nil {
			u32(rate * channels * bits / 8)
		} else {
			u32(rate * blockAlign)
		}
		u16(blockAlign)
		u16(bits)
		if s.Extensible {
			u16(22)
			u16(bits)
			u32(0)
			u16(tag)
			b = append(b, make([]byte, 14)...)
		}
	}
	data := func() {
		ascii("data")
		size := frames * channels * bits / 8
		u32(size)
		for i := 0; i < frames*channels; i++ {
			if tag == 3 {
				v := 0.0
				if !s.Silent {
					v = (float64(next()>>8)/16777216*2 - 1) * or(s.Amp, 0.5)
				}
				if s.NaNAt != nil && *s.NaNAt == i {
					v = math.NaN()
				}
				var tmp [8]byte
				if bits == 32 {
					binary.LittleEndian.PutUint32(tmp[:], math.Float32bits(float32(v)))
				} else {
					binary.LittleEndian.PutUint64(tmp[:], math.Float64bits(v))
				}
				b = append(b, tmp[:written]...)
				continue
			}
			for k := 0; k < written; k++ {
				switch {
				case s.Silent && bits == 8:
					u8(128)
				case s.Silent:
					u8(0)
				default:
					u8(int(next() >> 24))
				}
			}
		}
		if size%2 == 1 {
			u8(0)
		}
	}
	ascii("RIFF")
	u32(0)
	ascii("WAVE")
	if !s.NoFmt {
		fmtChunk()
	}
	for _, c := range s.PreChunks {
		ascii(c.ID)
		u32(c.Size)
		b = append(b, make([]byte, c.Size+c.Size%2)...)
	}
	if s.DupFmt {
		fmtChunk()
	}
	if !s.NoData {
		data()
	}
	if s.DupData {
		data()
	}
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	return b[:len(b)-s.Truncate]
}

type golden struct {
	Audio []struct {
		Spec        spec   `json:"spec"`
		InputSHA256 string `json:"inputSha256"`
		SHA256      string `json:"sha256"`
		SizeBytes   int    `json:"sizeBytes"`
		SampleCount int    `json:"sampleCount"`
		DurationMs  int64  `json:"durationMs"`
		Error       *Error `json:"error"`
	} `json:"audio"`
	Names []struct {
		Input string `json:"input"`
		Name  string `json:"name"`
		Error string `json:"error"`
		Key   string `json:"key"`
	} `json:"names"`
}

func (e *Error) UnmarshalJSON(b []byte) error {
	var v struct{ Code, Message string }
	err := json.Unmarshal(b, &v)
	e.Code, e.Message = v.Code, v.Message
	return err
}

func loadGolden(t *testing.T) *golden {
	t.Helper()
	b, err := os.ReadFile("testdata/voices.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return &g
}

func hexSHA(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// TestNormalizeAudioGolden reproduces the TS normalizer's output bytes and
// errors for every golden input.
func TestNormalizeAudioGolden(t *testing.T) {
	for _, c := range loadGolden(t).Audio {
		in := build(c.Spec)
		if got := hexSHA(in); got != c.InputSHA256 {
			t.Errorf("%s: input differs from the TS build (%s)", c.Spec.Case, got)
			continue
		}
		out, err := NormalizeAudio(in)
		if c.Error != nil {
			var ve *Error
			if !errors.As(err, &ve) || *ve != *c.Error {
				t.Errorf("%s: error %v, want %s %q", c.Spec.Case, err, c.Error.Code, c.Error.Message)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Spec.Case, err)
			continue
		}
		if hexSHA(out.WAV) != c.SHA256 || len(out.WAV) != c.SizeBytes || out.SampleCount != c.SampleCount || out.DurationMs != c.DurationMs {
			t.Errorf("%s: sha %s size %d samples %d duration %d; want %s %d %d %d", c.Spec.Case, hexSHA(out.WAV), len(out.WAV),
				out.SampleCount, out.DurationMs, c.SHA256, c.SizeBytes, c.SampleCount, c.DurationMs)
		}
	}
}

// TestNameRulesGolden matches the TS name schema and uniqueness key.
func TestNameRulesGolden(t *testing.T) {
	for _, c := range loadGolden(t).Names {
		name, err := ParseName(c.Input)
		switch {
		case c.Error != "":
			if err == nil || err.Error() != c.Error {
				t.Errorf("%q: got %q, %v; want error %q", c.Input, name, err, c.Error)
			}
		case err != nil || name != c.Name:
			t.Errorf("%q: got %q, %v; want %q", c.Input, name, err, c.Name)
		}
		key := c.Input
		if c.Error == "" {
			key = c.Name
		}
		if got := NameKey(key); got != c.Key {
			t.Errorf("key of %q: %q, want %q", key, got, c.Key)
		}
	}
}

func TestToFixed1(t *testing.T) {
	for x, want := range map[float64]string{0: "0.0", 0.05: "0.1", 2.25: "2.3", 2.35: "2.4", 30.05: "30.1", 1.0 / 192000: "0.0", 29.95: "29.9", 0.25: "0.3"} {
		if got := ToFixed1(x); got != want {
			t.Errorf("ToFixed1(%v) = %s, want %s", x, got, want)
		}
	}
}

// clip builds a valid 4 s 16 kHz PCM16 reference.
func clip(seed uint32) []byte {
	s := spec{Seed: &seed}
	return build(s)
}

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "localbase.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, dir), dir
}

func code(err error) string {
	var ve *Error
	if errors.As(err, &ve) {
		return ve.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func TestStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	s, dir := testStore(t)
	if _, err := s.Create(ctx, "Narrator", false, clip(1), "cli"); code(err) != "consent_required" {
		t.Fatalf("no consent: %v", err)
	}
	v, err := s.Create(ctx, "  Narrator ", true, clip(1), "api-key:k1")
	if err != nil {
		t.Fatal(err)
	}
	if !IDPattern.MatchString(v.ID) || v.Name != "Narrator" || v.DurationMs != 4000 || v.CreatedBy != "api-key:k1" ||
		v.Consent.Statement != ConsentStatementID || v.Consent.AttestedAt != v.CreatedAt {
		t.Fatalf("created %+v", v)
	}
	info, err := os.Stat(Dir(dir))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("voice dir: %v %v", info, err)
	}
	p, _ := Path(dir, v.ID)
	if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o600 || info.Size() != v.SizeBytes {
		t.Fatalf("reference file: %v %v", info, err)
	}
	if _, err := s.Create(ctx, "NARRATOR", true, clip(2), "cli"); code(err) != "voice_name_taken" {
		t.Fatalf("case-insensitive duplicate: %v", err)
	}
	if _, err := s.Create(ctx, "Default", true, clip(2), "cli"); code(err) != "invalid_voice_name" {
		t.Fatalf("reserved name: %v", err)
	}
	if _, err := s.Create(ctx, "Quiet", true, []byte("nope"), "cli"); code(err) != "unsupported_audio_format" {
		t.Fatalf("bad audio: %v", err)
	}
	if r, _ := s.Resolve(ctx, v.ID); r != p {
		t.Fatalf("resolve %q", r)
	}
	// A tampered file no longer resolves.
	os.WriteFile(p, []byte("RIFF"), 0o600)
	if r, _ := s.Resolve(ctx, v.ID); r != "" {
		t.Fatalf("tampered file resolved: %q", r)
	}
	if files, _ := s.ReferenceFiles(ctx); len(files) != 0 {
		t.Fatalf("tampered file listed: %v", files)
	}
	w, err := s.Create(ctx, "Second", true, clip(3), "cli")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rename(ctx, w.ID, "narrator"); code(err) != "voice_name_taken" {
		t.Fatalf("rename onto a taken name: %v", err)
	}
	if r, err := s.Rename(ctx, w.ID, "second"); err != nil || r.Name != "second" {
		t.Fatalf("rename to own key: %v %v", r, err)
	}
	if _, err := s.Rename(ctx, "voice-000000000000", "x"); code(err) != "voice_not_found" {
		t.Fatalf("rename missing: %v", err)
	}
	if list, _ := s.List(ctx); len(list) != 2 || list[0].ID != v.ID || list[1].Name != "second" {
		t.Fatalf("list %+v", list)
	}
	if _, err := s.Remove(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("file kept after remove: %v", err)
	}
	if _, err := s.Remove(ctx, v.ID); code(err) != "voice_not_found" {
		t.Fatalf("second remove: %v", err)
	}
	if g, _ := s.Get(ctx, "../etc/passwd"); g != nil {
		t.Fatal("path-shaped id")
	}
}

func TestStoreLimit(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	for i := 0; i < Limit; i++ {
		if _, err := s.Create(ctx, fmt.Sprintf("Voice %d", i), true, clip(uint32(i+1)), "cli"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Create(ctx, "One more", true, clip(99), "cli"); code(err) != "voice_limit_reached" {
		t.Fatalf("over the limit: %v", err)
	}
}
