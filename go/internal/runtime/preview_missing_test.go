package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/models"
)

// The LLM's preview answers a selected variant whose files aren't here as it answers a catalog model that
// isn't on disk: not installed, unless only the memory question is asked. That gets the demand it would
// get with the files here.
func TestAnLLMPreviewOfFilesThatArentHere(t *testing.T) {
	m := newTestLLM(t, &bigHost{avail: 50 * memory.GiB})
	ctx := context.Background()
	reg, err := backends.Load("")
	if err != nil {
		t.Fatal(err)
	}
	model := models.FromSpec(m.d.Catalog.ByID(modelA))
	target := &Target{ModelID: modelA, Model: &model, Variant: &model.Variants[0], Backend: reg.Get("llama.cpp-cpu"), Dir: m.d.ModelsDir}
	var gateErr error
	m.d.Gate = func(context.Context, string, bool) (*Target, error) { return target, gateErr }
	_, here, err := m.PreviewAdmission(ctx, modelA, false)
	if err != nil {
		t.Fatal(err)
	}
	gateErr = &NotInstalledError{ModelID: modelA}
	var missing *NotInstalledError
	if _, _, err := m.PreviewAdmission(ctx, modelA, false); !errors.As(err, &missing) {
		t.Fatalf("a preview of files that aren't here got %v; want not installed", err)
	}
	_, assumed, err := m.PreviewAdmission(ctx, modelA, true)
	if err != nil {
		t.Fatalf("the memory question alone got %v", err)
	}
	if assumed != here {
		t.Fatalf("the memory question alone got %+v; with the files here, %+v", assumed, here)
	}
}
