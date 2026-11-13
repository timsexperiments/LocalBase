package library

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/memory/memorytest"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// roomy is a computer with room for anything these tests ask about.
type roomy struct{}

func (roomy) Topology() memory.Topology {
	return memory.Topology{Unified: true, SystemCapacity: 64 * memory.GiB, GPUWorkingSetBytes: 48 * memory.GiB}
}

func (roomy) Snapshot(context.Context) memory.Snapshot {
	return memory.Snapshot{Available: 50 * memory.GiB, Pressure: "normal"}
}

// A preview of a model whose selected files aren't here sizes them by their pinned sizes, so it gets the
// same demand as once they're here. It answers "not installed" unless only the memory question is asked,
// and downloads nothing either way.
func TestAPreviewSizesMissingFilesAsTheyWillBe(t *testing.T) {
	l, asked := missingVariant(t)
	ctx := context.Background()
	mem := memory.NewController(memorytest.Unsampled{HostProvider: roomy{}}, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	cfg := &store.Config{Active: map[string]string{}, Selected: map[string][]string{"tts": {otherModel}}}
	svc := runtime.NewService(runtime.ServiceDeps{Modality: "tts", Kind: "tts", Catalog: l.Catalog, Memory: mem,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Config: func() *store.Config { return cfg },
		Gate: l.GateFor("speech"), ModelsDir: t.TempDir()})
	t.Cleanup(svc.Close)

	var missing *runtime.NotInstalledError
	if _, _, err := svc.PreviewAdmission(ctx, otherModel, false); !errors.As(err, &missing) {
		t.Fatalf("a preview of files that aren't here got %v; want not installed", err)
	}
	_, absent, err := svc.PreviewAdmission(ctx, otherModel, true)
	if err != nil {
		t.Fatalf("the memory question alone got %v", err)
	}

	// The variant's files, at their pinned sizes. Sparse: nothing is written.
	e, err := l.Get(ctx, otherModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, a := range e.Model.Variant(otherVariant).Artifacts {
		f, err := os.Create(filepath.Join(e.Dir, a.Filename))
		if err != nil {
			t.Fatal(err)
		}
		err = f.Truncate(a.SizeBytes)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	_, present, err := svc.PreviewAdmission(ctx, otherModel, false)
	if err != nil {
		t.Fatalf("a preview with the files here got %v", err)
	}
	if absent != present {
		t.Fatalf("sized %+v with the files missing, %+v with them here", absent, present)
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("previews asked for the model's files %d times", n)
	}
}
