package catalog

import "slices"

// supportedVideoTargets are the hosts LocalBase can run video on today, a new slice each call so
// nothing can change the list. Adding one means VideoHostRefusal has to change too;
// TestVideoRefusalsFollowTheTargets holds it to that.
func supportedVideoTargets() []string { return []string{"darwin/arm64/apple-unified"} }

const (
	// VideoHostRefusal is for a host that isn't a supported one.
	VideoHostRefusal = "Video generation runs only on Apple Silicon Macs today."
	// VideoModelRefusal is for a model with no target on any supported host.
	VideoModelRefusal = "This video model can't run on any computer LocalBase supports today. Choose another video model."
)

// VideoRunsAnywhere says whether a video model has a target LocalBase supports on any host.
func (s *Spec) VideoRunsAnywhere() bool { return s.videoRunsOn(supportedVideoTargets()) }

// videoRunsOn says whether a video model has a target among these (tests: another list).
func (s *Spec) videoRunsOn(targets []string) bool {
	if s.VideoRuntime == nil {
		return false
	}
	for _, t := range s.VideoRuntime.SupportedTargets {
		if slices.Contains(targets, t.Platform+"/"+t.Architecture+"/"+t.Accelerator) {
			return true
		}
	}
	return false
}

// VideoRefusal is why a video model can't run on a host without a matching target.
func (s *Spec) VideoRefusal() string { return s.videoRefusalOn(supportedVideoTargets()) }

func (s *Spec) videoRefusalOn(targets []string) string {
	if s.videoRunsOn(targets) {
		return VideoHostRefusal
	}
	return VideoModelRefusal
}
