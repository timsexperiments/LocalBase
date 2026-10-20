package backends

import (
	"fmt"
	"slices"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/models"
)

// Ratings, best first.
const (
	Supported    = "supported"    // qualified evidence: this backend runs this variant
	Likely       = "likely"       // documented or structurally matching, not qualified here
	Unknown      = "unknown"      // not enough metadata to say
	Incompatible = "incompatible" // a concrete known mismatch
)

var ratingRank = map[string]int{Supported: 3, Likely: 2, Unknown: 1, Incompatible: 0}

// Plausible ratings are the ones automatic selection may choose.
func Plausible(r string) bool { return r == Supported || r == Likely }

// Finding is one variant rated against one backend on this host.
type Finding struct {
	BackendID string   `json:"backendId"`
	VariantID string   `json:"variantId"`
	Rating    string   `json:"rating"`
	Reasons   []string `json:"reasons"`  // why this rating
	Warnings  []string `json:"warnings"` // shown with the option; never used to hide it
	// Drops lists required tasks or capabilities this backend does not
	// declare. Automatic selection never picks a finding that drops one.
	Drops []string `json:"drops,omitempty"`
	// Install is the backend's install state on this device.
	Install string `json:"install"`
	// Missing lists required files of the variant that are not present.
	Missing []string `json:"missing,omitempty"`
}

// Assess rates variant v of model m against backend d on host h. missing
// lists the variant's required files that are not present (from
// models.InspectVariant); downloading is a separate concern, so missing
// files only add a warning.
func Assess(m *models.Model, v *models.Variant, d *Definition, h Host, missing []string) Finding {
	f := Finding{BackendID: d.ID, VariantID: v.ID, Install: h.Install[d.ID], Missing: missing}
	var incompatible, unknown, likely []string
	supportedBy := ""

	// Tasks and capabilities the model needs.
	for _, need := range m.Required() {
		if !d.Supports(need) {
			f.Drops = append(f.Drops, need)
		}
	}
	taskOK := false
	for _, t := range m.Tasks {
		if slices.Contains(d.Tasks, t) {
			taskOK = true
		}
	}
	switch {
	case len(m.Tasks) == 0:
		unknown = append(unknown, "what the model does is not known yet")
	case !taskOK:
		incompatible = append(incompatible, fmt.Sprintf("does not run %s models", strings.Join(m.Tasks, "/")))
	case len(f.Drops) > 0:
		f.Warnings = append(f.Warnings, "does not declare "+strings.Join(f.Drops, ", ")+"; those features would not work")
	}

	// Platform and hardware.
	platformOK, accelOK := false, false
	var needAcc []string
	for _, p := range d.Platforms {
		if (p.OS == h.OS || p.OS == "*") && (p.Arch == h.Arch || p.Arch == "*") {
			platformOK = true
			if h.Has(p.Accelerator) {
				accelOK = true
			} else {
				needAcc = append(needAcc, p.Accelerator)
			}
		}
	}
	switch {
	case !platformOK:
		incompatible = append(incompatible, fmt.Sprintf("has no build for %s/%s", h.OS, h.Arch))
	case !accelOK:
		incompatible = append(incompatible, "needs "+strings.Join(needAcc, " or ")+", which this device does not have")
	}

	// Format: only a header-inspected or pinned fact counts.
	switch {
	case !v.Format.Known():
		unknown = append(unknown, "file format unknown")
	case !slices.Contains(d.Formats, v.Format.Value):
		incompatible = append(incompatible, fmt.Sprintf("reads %s, not %s", strings.Join(d.Formats, "/"), v.Format.Value))
	}

	// Architecture.
	arch := v.Architecture
	switch {
	case slices.Contains(v.QualifiedOn, d.ID):
		supportedBy = "qualified with this backend by the catalog"
	case !arch.Known():
		if arch.Conflict != "" {
			unknown = append(unknown, "architecture metadata conflicts: "+arch.Conflict)
		} else {
			unknown = append(unknown, "architecture unknown")
		}
	case slices.Contains(d.Architectures.Unsupported, arch.Value):
		incompatible = append(incompatible, fmt.Sprintf("does not support the %s architecture", arch.Value))
	case slices.Contains(d.Architectures.Supported, arch.Value):
		supportedBy = fmt.Sprintf("%s architecture is qualified with this backend", arch.Value)
	case slices.Contains(d.Architectures.Likely, arch.Value):
		likely = append(likely, fmt.Sprintf("%s architecture is documented upstream, not qualified here", arch.Value))
	default:
		unknown = append(unknown, fmt.Sprintf("%s architecture is not in this backend's definition", arch.Value))
	}

	// Precision, when the backend restricts it.
	if len(d.Precisions) > 0 && v.Precision.Known() && !slices.Contains(d.Precisions, v.Precision.Value) {
		incompatible = append(incompatible, fmt.Sprintf("does not read %s weights", v.Precision.Value))
	}

	// Companion files the backend needs for the model's tasks and capabilities.
	for _, need := range m.Required() {
		for _, role := range d.Companions[need] {
			if !slices.ContainsFunc(v.Artifacts, func(a models.Artifact) bool { return a.Role == role }) {
				incompatible = append(incompatible, fmt.Sprintf("%s needs a %s file, which this variant does not have", need, role))
			}
		}
	}

	// Runtime version constraint of the variant.
	if minV, ok := v.MinBackendVersion[d.ID]; ok && d.Version != "" && compareVersions(d.Version, minV) < 0 {
		incompatible = append(incompatible, fmt.Sprintf("needs version %s or newer, this backend is %s", minV, d.Version))
	}

	switch {
	case len(incompatible) > 0:
		f.Rating, f.Reasons = Incompatible, incompatible
	case len(unknown) > 0 && supportedBy == "":
		f.Rating, f.Reasons = Unknown, unknown
	case supportedBy != "":
		f.Rating, f.Reasons = Supported, append([]string{supportedBy}, likely...)
	default:
		f.Rating, f.Reasons = Likely, likely
	}
	switch f.Install {
	case Installable:
		f.Warnings = append(f.Warnings, "installs on first use")
	case Missing:
		f.Warnings = append(f.Warnings, "not installed on this device")
	}
	if len(missing) > 0 {
		f.Warnings = append(f.Warnings, "files not downloaded: "+strings.Join(missing, ", "))
	}
	return f
}

// compareVersions compares release tags such as b11671 or v1.2.3 by their
// numbers; non-numeric parts compare as strings.
func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

func versionParts(s string) []int {
	var out []int
	n, in := 0, false
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n, in = n*10+int(c-'0'), true
			continue
		}
		if in {
			out = append(out, n)
			n, in = 0, false
		}
	}
	if in {
		out = append(out, n)
	}
	return out
}
