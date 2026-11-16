package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// The second text says no supported computer can run the model. That comes from the model's targets
// and the supported list, so it can't stay true by accident.
func TestVideoRefusalsFollowTheTargets(t *testing.T) {
	const mac, nvidia = `{"platform":"darwin","architecture":"arm64","accelerator":"apple-unified"}`, `{"platform":"linux","architecture":"x64","accelerator":"nvidia"}`
	spec := func(targets ...string) *Spec {
		var rt VideoRuntime
		if err := json.Unmarshal([]byte(`{"supportedTargets":[`+strings.Join(targets, ",")+`]}`), &rt); err != nil {
			t.Fatal(err)
		}
		return &Spec{VideoRuntime: &rt}
	}
	if got := spec(nvidia).VideoRefusal(); got != VideoModelRefusal {
		t.Errorf("no supported target: %q", got)
	}
	if got := spec(nvidia, mac).VideoRefusal(); got != VideoHostRefusal {
		t.Errorf("gaining a supported target must end the second text: %q", got)
	}
	if got := spec(nvidia).videoRefusalOn([]string{"linux/x64/nvidia"}); got == VideoModelRefusal {
		t.Errorf("a host that became supported must end the second text: %q", got)
	}
}

// If this fails, a target was added: VideoHostRefusal names the hosts, so change it with the list.
func TestTheHostRefusalNamesTheSupportedHosts(t *testing.T) {
	if got := supportedVideoTargets(); len(got) != 1 || got[0] != "darwin/arm64/apple-unified" {
		t.Errorf("VideoHostRefusal says %q, and the list is %v", VideoHostRefusal, got)
	}
}

// Over the real catalog: each video model gets the text its targets call for.
func TestEveryCatalogVideoModelGetsTheRightRefusal(t *testing.T) {
	cat, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, s := range cat.All() {
		if s.VideoRuntime == nil {
			continue
		}
		seen++
		runs := false
		for _, target := range s.VideoRuntime.SupportedTargets {
			runs = runs || (target.Platform == "darwin" && target.Architecture == "arm64" && target.Accelerator == "apple-unified")
		}
		want := VideoModelRefusal
		if runs {
			want = VideoHostRefusal
		}
		if got := s.VideoRefusal(); got != want {
			t.Errorf("%s: %q, want %q", s.ModelID, got, want)
		}
	}
	if seen == 0 {
		t.Error("no video models in the catalog")
	}
}
