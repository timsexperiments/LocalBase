package catalog

import "slices"

// SupportedVideoTargets are the hosts LocalBase can run video on today. Adding one means
// VideoHostRefusal has to change too; TestVideoRefusalsFollowTheTargets holds it to that.
var SupportedVideoTargets = []string{"darwin/arm64/apple-unified"}

const (
	// VideoHostRefusal is for a host that isn't a supported one.
	VideoHostRefusal = "Video generation runs only on Apple Silicon Macs today."
	// VideoModelRefusal is for a model with no target on any supported host.
	VideoModelRefusal = "This video model can't run on any computer LocalBase supports today. Choose another video model."
)

// VideoRunsAnywhere says whether a video model has a target LocalBase supports on any host.
func (s *Spec) VideoRunsAnywhere() bool {
	if s.VideoRuntime == nil {
		return false
	}
	for _, t := range s.VideoRuntime.SupportedTargets {
		if slices.Contains(SupportedVideoTargets, t.Platform+"/"+t.Architecture+"/"+t.Accelerator) {
			return true
		}
	}
	return false
}

// VideoRefusal is why a video model can't run on a host without a matching target.
func (s *Spec) VideoRefusal() string {
	if s.VideoRunsAnywhere() {
		return VideoHostRefusal
	}
	return VideoModelRefusal
}
