package voices

import (
	"encoding/binary"
	"fmt"
	"math"
	"slices"
)

// Reference clips are decoded here, in bounded Go, instead of handing an
// untrusted file to a native decoder. Only RIFF/WAVE PCM and IEEE float are
// accepted; the UI converts recordings to WAV before upload.
//
// The arithmetic reproduces the TS normalizer byte for byte (a stored
// voice's sha256 is part of the API): samples and kernels are float32
// (Float32Array), sums are float64, and rounding is JavaScript's Math.round.

const (
	minInputRateHz = 8_000
	maxInputRateHz = 192_000
	maxChannels    = 8
	// Below roughly -60 dBFS peak the clip carries no usable speech.
	silencePeak = 0.001
	// Windowed-sinc half width, in zero crossings of the low-pass kernel.
	resampleZeroCrossings = 8

	formatPCM        = 0x0001
	formatFloat      = 0x0003
	formatExtensible = 0xfffe
)

// Normalized is a clip rewritten as 24 kHz mono PCM16 RIFF/WAVE with only
// `fmt ` and `data` chunks.
type Normalized struct {
	WAV         []byte
	SampleCount int
	DurationMs  int64
}

func unsupported(format string, a ...any) error {
	return &Error{Code: "unsupported_audio_format", Message: fmt.Sprintf(format, a...)}
}

type decodedWAV struct {
	sampleRate, channels, frames int
	read                         func(frame, channel int) (float64, error)
}

func decodeWAV(b []byte) (*decodedWAV, error) {
	if len(b) > MaxUploadBytes {
		return nil, &Error{Code: "audio_too_large", Message: fmt.Sprintf("Audio exceeds the %d MiB limit.", MaxUploadBytes/(1024*1024))}
	}
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, unsupported("Upload a WAV file (RIFF/WAVE).")
	}
	type wavFormat struct{ tag, channels, sampleRate, bits int }
	var format *wavFormat
	dataOffset, dataLength := -1, 0
	le := binary.LittleEndian
	for offset := 12; offset+8 <= len(b); {
		id := string(b[offset : offset+4])
		size := int(le.Uint32(b[offset+4:]))
		body := offset + 8
		if size > len(b)-body {
			return nil, unsupported("The WAV file is truncated or has an invalid chunk size.")
		}
		switch id {
		case "fmt ":
			if format != nil {
				return nil, unsupported("The WAV file has more than one format chunk.")
			}
			if size < 16 {
				return nil, unsupported("The WAV format chunk is too short.")
			}
			tag := int(le.Uint16(b[body:]))
			if tag == formatExtensible {
				if size < 40 {
					return nil, unsupported("The WAV format chunk is too short.")
				}
				// The sub-format GUID starts with the plain format tag.
				tag = int(le.Uint16(b[body+24:]))
			}
			channels := int(le.Uint16(b[body+2:]))
			sampleRate := int(le.Uint32(b[body+4:]))
			blockAlign := int(le.Uint16(b[body+12:]))
			bits := int(le.Uint16(b[body+14:]))
			if tag != formatPCM && tag != formatFloat {
				return nil, unsupported("Only uncompressed PCM or float WAV audio is supported.")
			}
			valid := bits == 32 || bits == 64
			if tag == formatPCM {
				valid = slices.Contains([]int{8, 16, 24, 32}, bits)
			}
			if !valid {
				return nil, unsupported("%d-bit WAV audio is not supported.", bits)
			}
			if channels < 1 || channels > maxChannels {
				return nil, unsupported("WAV audio must have 1 to %d channels.", maxChannels)
			}
			if sampleRate < minInputRateHz || sampleRate > maxInputRateHz {
				return nil, unsupported("WAV sample rate must be between %d and %d Hz.", minInputRateHz, maxInputRateHz)
			}
			// TS compares against channels * (bits / 8); bits is a multiple of 8 here.
			if blockAlign != channels*(bits/8) {
				return nil, unsupported("The WAV block alignment does not match its format.")
			}
			format = &wavFormat{tag, channels, sampleRate, bits}
		case "data":
			if dataOffset >= 0 {
				return nil, unsupported("The WAV file has more than one data chunk.")
			}
			dataOffset, dataLength = body, size
		}
		// Chunks are word aligned; a final odd chunk may omit its pad byte.
		offset = body + size + size%2
	}
	if format == nil {
		return nil, unsupported("The WAV file has no format chunk.")
	}
	if dataOffset < 0 {
		return nil, unsupported("The WAV file has no audio data.")
	}
	bytesPerSample := format.bits / 8
	blockBytes := format.channels * bytesPerSample
	read := func(frame, channel int) (float64, error) {
		at := dataOffset + frame*blockBytes + channel*bytesPerSample
		if format.tag == formatFloat {
			var v float64
			if format.bits == 32 {
				v = float64(math.Float32frombits(le.Uint32(b[at:])))
			} else {
				v = math.Float64frombits(le.Uint64(b[at:]))
			}
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return 0, unsupported("The WAV file contains invalid float samples.")
			}
			return max(-1, min(1, v)), nil
		}
		switch format.bits {
		case 8:
			return (float64(b[at]) - 128) / 128, nil
		case 16:
			return float64(int16(le.Uint16(b[at:]))) / 32_768, nil
		case 24:
			v := int32(b[at]) | int32(b[at+1])<<8 | int32(int8(b[at+2]))<<16
			return float64(v) / 8_388_608, nil
		}
		return float64(int32(le.Uint32(b[at:]))) / 2_147_483_648, nil
	}
	return &decodedWAV{sampleRate: format.sampleRate, channels: format.channels, frames: dataLength / blockBytes, read: read}, nil
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// resampleMono is polyphase windowed-sinc resampling. Rates are integers, so
// the output grid repeats every `up` samples and each phase has one kernel.
func resampleMono(input []float32, inputRate, outputRate int) []float32 {
	if inputRate == outputRate {
		return slices.Clone(input)
	}
	d := gcd(inputRate, outputRate)
	up, down := outputRate/d, inputRate/d
	cutoff := math.Min(1, float64(outputRate)/float64(inputRate))
	halfWidth := int(math.Ceil(resampleZeroCrossings / cutoff))
	taps := 2 * halfWidth
	kernels := make([]float32, up*taps)
	for phase := 0; phase < up; phase++ {
		fraction := float64(phase) / float64(up)
		sum := 0.0
		for tap := 0; tap < taps; tap++ {
			distance := float64(tap-halfWidth+1) - fraction
			x := cutoff * distance
			sinc := 1.0
			if x != 0 {
				sinc = math.Sin(math.Pi*x) / (math.Pi * x)
			}
			// Blackman window over the kernel span.
			position := (distance + float64(halfWidth)) / float64(2*halfWidth)
			window := 0.42 - 0.5*math.Cos(2*math.Pi*position) + 0.08*math.Cos(4*math.Pi*position)
			value := sinc * math.Max(0, window)
			kernels[phase*taps+tap] = float32(value)
			sum += value
		}
		// Unity DC gain for every phase.
		for tap := 0; tap < taps; tap++ {
			kernels[phase*taps+tap] = float32(float64(kernels[phase*taps+tap]) / sum)
		}
	}
	length := len(input) * up / down
	output := make([]float32, length)
	for index := 0; index < length; index++ {
		position := index * down
		base, phase := position/up, position%up
		kernel := kernels[phase*taps : (phase+1)*taps]
		value := 0.0
		for tap := 0; tap < taps; tap++ {
			if sample := base + tap - halfWidth + 1; sample >= 0 && sample < len(input) {
				value += float64(input[sample]) * float64(kernel[tap])
			}
		}
		output[index] = float32(value)
	}
	return output
}

// jsRound is JavaScript's Math.round: the nearest integer, ties toward +∞.
func jsRound(x float64) float64 {
	r := math.Floor(x)
	if x-r >= 0.5 {
		r++
	}
	return r
}

// ToFixed1 is JavaScript's Number.prototype.toFixed(1) for non-negative
// finite values: exact decimal value, ties toward the larger number.
func ToFixed1(x float64) string {
	digits := fmt.Sprintf("%.80f", x) // exact: a double above 2^-28 has at most 80 fractional digits
	dot := len(digits) - 81
	head, rest := digits[:dot+2], digits[dot+2:]
	n := 0
	for _, c := range head {
		if c != '.' {
			n = n*10 + int(c-'0')
		}
	}
	if rest[0] >= '5' {
		n++
	}
	return fmt.Sprintf("%d.%d", n/10, n%10)
}

func encodePCM16WAV(samples []float32, sampleRate int) []byte {
	b := make([]byte, 44+len(samples)*2)
	le := binary.LittleEndian
	copy(b[0:], "RIFF")
	le.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVE")
	copy(b[12:], "fmt ")
	le.PutUint32(b[16:], 16)
	le.PutUint16(b[20:], formatPCM)
	le.PutUint16(b[22:], 1)
	le.PutUint32(b[24:], uint32(sampleRate))
	le.PutUint32(b[28:], uint32(sampleRate*2))
	le.PutUint16(b[32:], 2)
	le.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	le.PutUint32(b[40:], uint32(len(samples)*2))
	for i, s := range samples {
		v := max(-1, min(1, float64(s)))
		scaled := v * 32_767
		if v < 0 {
			scaled = v * 32_768
		}
		le.PutUint16(b[44+i*2:], uint16(int16(jsRound(scaled))))
	}
	return b
}

// NormalizeAudio validates an uploaded reference clip and rewrites it as
// 24 kHz mono PCM16, which also drops every metadata chunk it carried.
func NormalizeAudio(b []byte) (*Normalized, error) {
	d, err := decodeWAV(b)
	if err != nil {
		return nil, err
	}
	seconds := float64(d.frames) / float64(d.sampleRate)
	if seconds < MinSeconds {
		return nil, &Error{Code: "audio_too_short", Message: fmt.Sprintf("The recording is %s s; use at least %d s of speech.", ToFixed1(seconds), MinSeconds)}
	}
	if seconds > MaxSeconds {
		return nil, &Error{Code: "audio_too_long", Message: fmt.Sprintf("The recording is %s s; keep it to %d s or less.", ToFixed1(seconds), MaxSeconds)}
	}
	mono := make([]float32, d.frames)
	for frame := range mono {
		sum := 0.0
		for ch := 0; ch < d.channels; ch++ {
			v, err := d.read(frame, ch)
			if err != nil {
				return nil, err
			}
			sum += v
		}
		mono[frame] = float32(sum / float64(d.channels))
	}
	resampled := resampleMono(mono, d.sampleRate, SampleRateHz)
	peak := 0.0
	for _, s := range resampled {
		peak = math.Max(peak, math.Abs(float64(s)))
	}
	if peak < silencePeak {
		return nil, &Error{Code: "audio_silent", Message: "The recording is silent. Check the microphone and record again."}
	}
	return &Normalized{
		WAV:         encodePCM16WAV(resampled, SampleRateHz),
		SampleCount: len(resampled),
		DurationMs:  int64(jsRound(float64(len(resampled)) / SampleRateHz * 1000)),
	}, nil
}
