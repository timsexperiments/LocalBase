package gateway

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestPromptWithTuning matches TS promptWithImageTuning output exactly
// (strings recorded from the TS function).
func TestPromptWithTuning(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	i := func(v int64) *int64 { return &v }
	cases := []struct {
		prompt string
		tuning imageTuning
		lora   *imageLora
		want   string
	}{
		{"a cat", imageTuning{}, nil, "a cat"},
		{"a cat", imageTuning{negativePrompt: `blur <b> & "q"`, seed: i(42), steps: i(6), cfgScale: f(7.5), strength: f(0.25)},
			&imageLora{"/m/lora.safetensors", 0.8},
			`a cat <sd_cpp_extra_args>{"lora":[{"path":"/m/lora.safetensors","multiplier":0.8,"is_high_noise":false}],"negative_prompt":"blur <b> & \"q\"","seed":42,"strength":0.25,"sample_params":{"sample_steps":6,"guidance":{"txt_cfg":7.5}}}</sd_cpp_extra_args>`},
		{"x", imageTuning{cfgScale: f(1e21), seed: i(0)}, nil, `x <sd_cpp_extra_args>{"seed":0,"sample_params":{"guidance":{"txt_cfg":1e+21}}}</sd_cpp_extra_args>`},
		{"x", imageTuning{steps: i(3)}, nil, `x <sd_cpp_extra_args>{"sample_params":{"sample_steps":3}}</sd_cpp_extra_args>`},
	}
	for _, c := range cases {
		if got := promptWithTuning(c.prompt, c.tuning, c.lora); got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}
}

func TestWAVSamples(t *testing.T) {
	wav := func(rate uint32, chunks ...[]byte) []byte {
		var b bytes.Buffer
		b.WriteString("RIFF\x00\x00\x00\x00WAVE")
		fmtc := make([]byte, 16)
		binary.LittleEndian.PutUint16(fmtc, 1)
		binary.LittleEndian.PutUint16(fmtc[2:], 1)
		binary.LittleEndian.PutUint32(fmtc[4:], rate)
		binary.LittleEndian.PutUint16(fmtc[14:], 16)
		chunk := func(id string, data []byte) {
			b.WriteString(id)
			binary.Write(&b, binary.LittleEndian, uint32(len(data)))
			b.Write(data)
			if len(data)%2 == 1 {
				b.WriteByte(0)
			}
		}
		chunk("fmt ", fmtc)
		for _, c := range chunks {
			chunk(string(c[:4]), c[4:])
		}
		return b.Bytes()
	}
	samples := []byte{1, 2, 3, 4}
	// A C2PA manifest chunk (odd-sized, padded) before the samples is skipped.
	if got := wavSamples(wav(24000, []byte("C2PAabc"), append([]byte("data"), samples...))); !bytes.Equal(got, samples) {
		t.Errorf("samples %v", got)
	}
	if wavSamples(wav(22050, append([]byte("data"), samples...))) != nil {
		t.Error("wrong rate accepted")
	}
	if wavSamples([]byte("RIFF")) != nil || wavSamples(wav(24000)) != nil {
		t.Error("malformed accepted")
	}
}
