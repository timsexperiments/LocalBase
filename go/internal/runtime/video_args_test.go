package runtime

import (
	"os"
	"os/exec"
	"reflect"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// contractPlan is the launch the TS sd-video-launch-contract checked against the pinned binary:
// S2V with every launch option, on a discrete 12 GiB GPU with an 8 GiB accelerator demand.
func contractPlan() sdVideoPlan {
	dir := "/tmp/localbase-video-launch-contract/"
	return sdVideoPlan{mode: "s2v", decoderKind: "vae",
		diffusion: dir + "diffusion.gguf", textEncoder: dir + "t5xxl.gguf", decoder: dir + "vae.safetensors",
		audioEncoder: dir + "wav2vec2.safetensors",
		cpuOffload:   true, flashAttention: true, vaeConvDirect: false,
		host: "127.0.0.1", port: 8091, discrete: true, acceleratorBytes: 8 * memory.GiB}
}

func TestSDVideoArgs(t *testing.T) {
	dir := "/tmp/localbase-video-launch-contract/"
	want := []string{"--diffusion-model", dir + "diffusion.gguf", "--t5xxl", dir + "t5xxl.gguf",
		"--vae", dir + "vae.safetensors", "--audio-encoder", dir + "wav2vec2.safetensors",
		"--offload-to-cpu", "--diffusion-fa", "--listen-ip", "127.0.0.1", "--listen-port", "8091", "--max-vram", "8"}
	if got := sdVideoArgs(contractPlan()); !reflect.DeepEqual(got, want) {
		t.Errorf("contract plan:\n got %q\nwant %q", got, want)
	}

	// T2V on unified memory with a TAE decoder: no audio encoder, no --max-vram.
	p := contractPlan()
	p.mode, p.decoderKind, p.audioEncoder, p.vaeConvDirect, p.discrete = "t2v", "tae", "", true, false
	want = []string{"--diffusion-model", dir + "diffusion.gguf", "--t5xxl", dir + "t5xxl.gguf",
		"--tae", dir + "vae.safetensors", "--offload-to-cpu", "--diffusion-fa", "--vae-conv-direct",
		"--listen-ip", "127.0.0.1", "--listen-port", "8091"}
	if got := sdVideoArgs(p); !reflect.DeepEqual(got, want) {
		t.Errorf("t2v tae unified:\n got %q\nwant %q", got, want)
	}

	// A fractional accelerator demand is written as JS writes the number.
	p = contractPlan()
	p.acceleratorBytes = 6*memory.GiB + memory.GiB/2
	if got := sdVideoArgs(p); got[len(got)-1] != "6.5" {
		t.Errorf("--max-vram %s, want 6.5", got[len(got)-1])
	}
}

// TestSDServerAcceptsVideoArgs runs a pinned sd-server with the contract plan's arguments and
// --help: it must accept them, and must refuse an argument it doesn't know (so --help alone isn't
// what passes). release-sd-server.yml runs it against the binary it builds.
func TestSDServerAcceptsVideoArgs(t *testing.T) {
	bin := os.Getenv("LOCALBASE_SD_SERVER_BINARY")
	if bin == "" {
		t.Skip("set LOCALBASE_SD_SERVER_BINARY to a built sd-server to check it accepts the video launch arguments")
	}
	args := sdVideoArgs(contractPlan())
	if out, err := exec.Command(bin, append(args, "--help")...).CombinedOutput(); err != nil {
		t.Fatalf("the pinned sd-server rejected LocalBase's video launch arguments: %v\n%s", err, out)
	}
	if out, err := exec.Command(bin, append(args, "--localbase-video-launch-contract-invalid", "--help")...).CombinedOutput(); err == nil {
		t.Fatalf("the pinned sd-server accepted an unknown argument, so it isn't checking them:\n%s", out)
	}
}
