package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// linkCrispASR prepares Kokoro through CrispASR in a scratch root from
// LOCALBASE_CONTRACT_CRISPASR: rt/crispasr-macos-arm64/ (the pinned release,
// unpacked), misaki-merged.json and models/kokoro-*.gguf. The runtime is
// copied into the managed layout; the weights are cloned (APFS) read-only.
func linkCrispASR(r *root.Root) error {
	src := os.Getenv("LOCALBASE_CONTRACT_CRISPASR")
	if src == "" || goruntime.GOOS != "darwin" || goruntime.GOARCH != "arm64" {
		return nil
	}
	pkg := filepath.Join(r.RuntimesDir(), "crispasr", crispasrPin)
	if err := fsguard.MkdirAll(pkg, 0o700); err != nil {
		return err
	}
	if err := fsguard.CopyDir(filepath.Join(src, "rt", "crispasr-macos-arm64"), pkg); err != nil {
		return err
	}
	if err := fsguard.CloneModel(filepath.Join(src, "misaki-merged.json"), filepath.Join(pkg, "misaki-merged.json")); err != nil {
		return err
	}
	if err := fsguard.MkdirAll(r.ModelsDir("tts"), 0o700); err != nil {
		return err
	}
	for _, f := range []string{"kokoro-82m-f16.gguf", "kokoro-voice-af_heart.gguf"} {
		if err := fsguard.CloneModel(filepath.Join(src, "models", f), filepath.Join(r.ModelsDir("tts"), f)); err != nil {
			return err
		}
	}
	return nil
}

// crispasrPin is the managed CrispASR package directory (its archive sha256).
const crispasrPin = "5c13fbbc12eb45c3f01823a6bbfbbb22e0682bc76c6b78a0d9511da1aaf88831"

// TinyModel is a user catalog entry whose 4 KiB file is already in place, so
// an install completes through checksum verification without a network.
const TinyModel = "contract-tiny-stt"

func tinyBytes() []byte {
	b := make([]byte, 4096)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func addTinyCatalogModel(rootDir, sttDir string) error {
	data := tinyBytes()
	sum := sha256.Sum256(data)
	entry := map[string]any{
		"modelId": TinyModel, "displayName": "Contract Tiny", "kind": "stt", "provider": "LocalBase tests", "family": "Whisper",
		"version": "test", "size": "tiny", "quant": "Q8_0", "storageGb": 0.01,
		"source": "https://huggingface.co/localbase/contract-tiny", "repositoryRevision": strings.Repeat("a", 40),
		"artifacts": []any{map[string]any{"sourcePath": "ggml-contract-tiny.bin", "filename": "ggml-contract-tiny.bin",
			"expectedSizeBytes": len(data), "sha256": hex.EncodeToString(sum[:]), "role": "primary"}},
		"inputModalities": []string{"audio"}, "outputModalities": []string{"text"}, "capabilities": []string{"speech-to-text"},
		"commercialStatus": "open", "catch": "Test data.", "notes": "Contract fixture.",
	}
	b, _ := json.MarshalIndent(entry, "", "  ")
	if err := fsguard.MkdirAll(filepath.Join(rootDir, "catalog.d"), 0o700); err != nil {
		return err
	}
	if err := fsguard.WriteFile(filepath.Join(rootDir, "catalog.d", TinyModel+".json"), b, 0o600); err != nil {
		return err
	}
	fsguard.MkdirAll(sttDir, 0o700)
	return fsguard.WriteFile(filepath.Join(sttDir, "ggml-contract-tiny.bin"), data, 0o600)
}

// imageFile is dreamshaper-v8's checkpoint, the smallest catalog image model
// with image-to-image.
const imageFile = "DreamShaper_8_pruned.safetensors"
