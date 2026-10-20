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
	Reasons   []Reason `json:"reasons"`  // why this rating
	Warnings  []Reason `json:"warnings"` // shown with the option; never used to hide it
	// Drops lists required tasks or capabilities this backend does not
	// declare. Automatic selection never picks a finding that drops one.
	Drops []string `json:"drops,omitempty"`
	// Install is the backend's install state on this device.
	Install string `json:"install"`
	// Missing lists required files of the variant that are not present.
	Missing []string `json:"missing,omitempty"`
}

// Reason is one finding: a stable machine code for the API and a sentence
// for people.
type Reason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Reason codes.
const (
	CodeQualifiedCatalog   = "qualified-by-catalog"
	CodeArchQualified      = "architecture-qualified"
	CodeArchDocumented     = "architecture-documented-upstream"
	CodeArchUnlisted       = "architecture-not-in-definition"
	CodeArchUnknown        = "architecture-unknown"
	CodeArchConflict       = "architecture-conflicting-metadata"
	CodeArchUnsupported    = "architecture-unsupported"
	CodeArchWeakEvidence   = "architecture-evidence-insufficient"
	CodeTaskUnknown        = "task-unknown"
	CodeTaskUnsupported    = "task-unsupported"
	CodeCapabilityMissing  = "capability-not-declared"
	CodeNoPlatformBuild    = "no-build-for-platform"
	CodeNoAccelerator      = "accelerator-not-present"
	CodeFormatUnknown      = "format-unknown"
	CodeFormatUnsupported  = "format-unsupported"
	CodePrecision          = "precision-unsupported"
	CodeCompanionMissing   = "companion-file-missing"
	CodeVersionTooOld      = "backend-version-too-old"
	CodeInstallsOnFirstUse = "installs-on-first-use"
	CodeNotInstalled       = "not-installed"
	CodeNoManagedPackage   = "no-managed-package"
	CodeFilesMissing       = "files-not-downloaded"
	CodeManualOnly         = "not-chosen-automatically"
)

// evidence is where a fact may come from to count for a rating: never a
// file name or extension.
var evidence = []string{models.SourcePinned, models.SourceDeclared, models.SourceInspect, models.SourceVerified}

// Assess rates variant v of model m against backend d on host h. missing
// lists the variant's required files that are not present (from
// models.InspectVariant); downloading is a separate concern, so missing
// files only add a warning.
func Assess(m *models.Model, v *models.Variant, d *Definition, h Host, missing []string) Finding {
	f := Finding{BackendID: d.ID, VariantID: v.ID, Install: h.Install[d.ID], Missing: missing,
		Reasons: []Reason{}, Warnings: []Reason{}}
	var incompatible, unknown, likely []Reason
	var supportedBy *Reason
	r := func(code, format string, a ...any) Reason { return Reason{code, fmt.Sprintf(format, a...)} }
	warn := func(code, format string, a ...any) { f.Warnings = append(f.Warnings, r(code, format, a...)) }

	// Tasks and capabilities the model needs.
	for _, need := range m.Required() {
		if !d.Supports(need) {
			f.Drops = append(f.Drops, need)
		}
	}
	taskOK := slices.ContainsFunc(m.Tasks, func(t string) bool { return slices.Contains(d.Tasks, t) })
	switch {
	case len(m.Tasks) == 0:
		unknown = append(unknown, r(CodeTaskUnknown, "It is not known yet what this model does."))
	case !taskOK:
		incompatible = append(incompatible, r(CodeTaskUnsupported, "This backend does not run %s models.", strings.Join(m.Tasks, " or ")))
	case len(f.Drops) > 0:
		warn(CodeCapabilityMissing, "This backend does not declare %s, so those features would not work.", strings.Join(f.Drops, ", "))
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
		incompatible = append(incompatible, r(CodeNoPlatformBuild, "There is no build of this backend for %s/%s.", h.OS, h.Arch))
	case !accelOK:
		incompatible = append(incompatible, r(CodeNoAccelerator, "This build needs %s, which this device does not have.", strings.Join(needAcc, " or ")))
	}

	// Format: a fact from the file or a pinned record, never the name.
	switch {
	case !v.Format.Known() || !slices.Contains(evidence, v.Format.Source):
		unknown = append(unknown, r(CodeFormatUnknown, "The file format is not known yet."))
	case !slices.Contains(d.Formats, v.Format.Value):
		incompatible = append(incompatible, r(CodeFormatUnsupported, "This backend reads %s files, and this model is %s.", strings.Join(d.Formats, " or "), v.Format.Value))
	}

	// Architecture.
	arch := v.Architecture
	switch {
	case slices.Contains(v.QualifiedOn, d.ID):
		x := r(CodeQualifiedCatalog, "The built-in catalog qualified this model with this backend.")
		supportedBy = &x
	case arch.Conflict != "":
		unknown = append(unknown, r(CodeArchConflict, "The model's metadata disagrees about its architecture (%s).", arch.Conflict))
	case !arch.Known():
		unknown = append(unknown, r(CodeArchUnknown, "The model's architecture is not known yet."))
	case !slices.Contains(evidence, arch.Source):
		unknown = append(unknown, r(CodeArchWeakEvidence, "The architecture %s is not backed by the file or a pinned record.", arch.Value))
	case slices.Contains(d.Architectures.Unsupported, arch.Value):
		incompatible = append(incompatible, r(CodeArchUnsupported, "This backend does not support the %s architecture.", arch.Value))
	case slices.Contains(d.Architectures.Supported, arch.Value):
		x := r(CodeArchQualified, "The %s architecture was qualified with this backend.", arch.Value)
		supportedBy = &x
	case slices.Contains(d.Architectures.Likely, arch.Value):
		likely = append(likely, r(CodeArchDocumented, "The %s architecture is documented to work with this backend, but was not qualified here.", arch.Value))
	default:
		unknown = append(unknown, r(CodeArchUnlisted, "This backend's definition does not mention the %s architecture.", arch.Value))
	}

	// Precision, when the backend restricts it.
	if len(d.Precisions) > 0 && v.Precision.Known() && !slices.Contains(d.Precisions, v.Precision.Value) {
		incompatible = append(incompatible, r(CodePrecision, "This backend does not read %s weights.", v.Precision.Value))
	}

	// Companion files the backend needs for the model's tasks and capabilities.
	for _, need := range m.Required() {
		for _, role := range d.Companions[need] {
			if !slices.ContainsFunc(v.Artifacts, func(a models.Artifact) bool { return a.Role == role }) {
				incompatible = append(incompatible, r(CodeCompanionMissing, "For %s this backend needs a %s file, which this variant does not have.", need, role))
			}
		}
	}

	// Runtime version constraint of the variant.
	if minV, ok := v.MinBackendVersion[d.ID]; ok && d.Version != "" && compareVersions(d.Version, minV) < 0 {
		incompatible = append(incompatible, r(CodeVersionTooOld, "This variant needs backend version %s or newer; this backend is %s.", minV, d.Version))
	}

	switch {
	case len(incompatible) > 0:
		f.Rating, f.Reasons = Incompatible, incompatible
	case len(unknown) > 0 && supportedBy == nil:
		f.Rating, f.Reasons = Unknown, unknown
	case supportedBy != nil:
		f.Rating, f.Reasons = Supported, append([]Reason{*supportedBy}, likely...)
	default:
		f.Rating, f.Reasons = Likely, likely
	}
	switch f.Install {
	case Installable:
		warn(CodeInstallsOnFirstUse, "LocalBase installs this backend the first time it is used.")
	case Missing:
		if h.Unpackaged[d.ID] {
			warn(CodeNoManagedPackage, "Not installed: LocalBase has no package of this backend for this device yet.")
		} else {
			warn(CodeNotInstalled, "Not installed on this device.")
		}
	}
	switch {
	case !d.automatic():
		warn(CodeManualOnly, "LocalBase never picks this backend automatically; you can still choose it.")
	case !d.automaticFor(m):
		warn(CodeManualOnly, "LocalBase picks this backend automatically only for %s, which it has qualified; you can still choose it.", strings.Join(d.AutomaticTasks, ", "))
	}
	if len(missing) > 0 {
		warn(CodeFilesMissing, "These files are not downloaded yet: %s.", strings.Join(missing, ", "))
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

// Messages returns the people-facing sentences of reasons.
func Messages(rs []Reason) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Message)
	}
	return out
}
