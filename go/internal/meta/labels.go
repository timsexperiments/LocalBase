package meta

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// Plain-language wording shared by `models show` and the browser UI
// (model-details.ts, capabilities.ts).

var (
	ModeLabels    = map[string]string{"llm": "Chat", "image": "Image", "tts": "Speech", "stt": "Transcription", "video": "Video", "embedding": "Embeddings", "decision": "Decision"}
	LicenseLabels = map[string]string{"open": "Commercial use OK", "conditional": "Commercial use with conditions", "prohibited": "Non-commercial only"}
	TrustLabels   = map[string]string{"qualified": "Qualified by LocalBase", "community": "Community catalog, not qualified",
		"user": "Added by you, not qualified", "modified": "Qualified entry you changed; reference measurements no longer apply"}
	TierLabels         = map[string]string{"built-in": "Built-in catalog", "remote": "Remote catalog", "user": "Your catalog"}
	MemoryFigureLabels = map[string]string{"estimate": "Estimate", "measured": "Measured", "feed": "Measured (catalog feed)", "observed": "Seen on this machine"}
	FitLabels          = map[string]string{"fits": "Fits", "tight": "Tight fit", "wont-fit": "Won't fit"}
	SourceLabels       = map[string]string{"declared": "Listed in the catalog", "detected": "Found in the installed files", "runtime": "Reported by the running engine"}
	CapabilityLabels   = map[string]string{"text-generation": "Text generation", "tool-calling": "Tool calling", "reasoning": "Reasoning",
		"embeddings": "Embeddings", "decision": "Decisions", "code-generation": "Code generation", "code-editing": "Code editing",
		"code-retrieval": "Code retrieval", "agentic-coding": "Agentic coding", "long-context": "Long context",
		"multilingual": "Multilingual", "translation": "Translation", "speech-to-text": "Speech to text",
		"text-to-speech": "Text to speech", "text-to-image": "Text to image", "image-to-image": "Image editing",
		"text-to-video": "Text to video", "image-to-video": "Image to video", "speech-to-video": "Speech to video",
		"mixture-of-experts": "Mixture of experts"}
)

func CapabilityLabel(id string) string {
	if l, ok := CapabilityLabels[id]; ok {
		return l
	}
	return id
}

const gib = 1 << 30

// FormatGB is bytes as GB with one decimal.
func FormatGB(b int64) string { return fmt.Sprintf("%.1f GB", float64(b)/gib) }

// FormatFileSize scales small files so they do not show as 0.0 GB.
func FormatFileSize(b *int64) string {
	switch {
	case b == nil:
		return "Unknown size"
	case *b < 1<<20:
		return fmt.Sprintf("%d KB", max(1, int(math.Round(float64(*b)/1024))))
	case *b < gib:
		return fmt.Sprintf("%.1f MB", float64(*b)/(1<<20))
	}
	return FormatGB(*b)
}

// Thousands formats n with comma separators, as toLocaleString("en-US").
func Thousands(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func FormatTokens(n int) string { return Thousands(n) + " tokens" }

// DescribeLaunchShape puts a launch-shape key in words; unknown keys are
// returned unchanged.
func DescribeLaunchShape(shape string) string {
	if shape == "default" {
		return "standard settings"
	}
	parts := map[string]string{}
	for _, p := range strings.Split(shape, ";") {
		k, v, _ := strings.Cut(p, "=")
		parts[k] = v
	}
	ctx, err1 := strconv.Atoi(parts["ctx"])
	slots, err2 := strconv.Atoi(parts["slots"])
	kv := parts["kv"]
	if err1 != nil || err2 != nil || kv == "" {
		return shape
	}
	k, v, _ := strings.Cut(kv, "/")
	cache := kv
	if k == v {
		cache = k
	}
	req := "requests"
	if slots == 1 {
		req = "request"
	}
	return fmt.Sprintf("%s-token context, %d %s at a time, %s cache", Thousands(ctx), slots, req, cache)
}

func round1(v float64) string { return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) }

// DescribeFit is the fit with the memory left over, or how far short it falls.
func DescribeFit(verdict string, systemGB int, headroom float64) string {
	spare := round1(headroom) + " GB to spare"
	if headroom < 0 {
		spare = round1(-headroom) + " GB short"
	}
	return fmt.Sprintf("%s, %s of %s GB", FitLabels[verdict], spare, round1(float64(systemGB)))
}

var sentenceEnd = regexp.MustCompile(`[.!?]$`)

// DescribeMismatch explains disagreeing capability sources.
func DescribeMismatch(m runtime.Mismatch) string {
	facts := []string{"the catalog doesn't list it"}
	if m.Declared {
		facts[0] = "the catalog lists it"
	}
	if m.Detected != nil {
		facts = append(facts, map[bool]string{true: "the installed files support it", false: "the installed files don't support it"}[*m.Detected])
	}
	if m.Runtime != nil {
		facts = append(facts, map[bool]string{true: "the running engine reports it", false: "the running engine doesn't report it"}[*m.Runtime])
	}
	joined := facts[0] + ", but " + facts[len(facts)-1]
	if len(facts) != 2 {
		joined = strings.Join(facts[:len(facts)-1], ", ") + ", and " + facts[len(facts)-1]
	}
	label := CapabilityLabel(m.ID)
	if m.UnsupportedReason != "" {
		if m.Effective {
			return fmt.Sprintf("%s: the catalog marks it verified unsupported (%s), but a runtime round-trip succeeded. Treated as available, going by the round-trip.", label, m.UnsupportedReason)
		}
		var claims []string
		if m.Detected != nil && *m.Detected {
			claims = append(claims, "the installed files claim it")
		}
		if m.Runtime != nil && *m.Runtime {
			claims = append(claims, "the engine's template flags claim it")
		}
		despite := ""
		if len(claims) > 0 {
			despite = " This holds even though " + strings.Join(claims, " and ") + "."
		}
		end := "."
		if sentenceEnd.MatchString(m.UnsupportedReason) {
			end = ""
		}
		return fmt.Sprintf("%s: verified unsupported. %s%s%s Treated as unavailable until a runtime round-trip proves otherwise.", label, m.UnsupportedReason, end, despite)
	}
	decider := "the catalog"
	switch {
	case m.Verified != nil:
		decider = "a runtime round-trip"
	case m.Runtime != nil:
		decider = "the running engine"
	case m.Detected != nil:
		decider = "the installed files"
	}
	state := "unavailable"
	if m.Effective {
		state = "available"
	}
	return fmt.Sprintf("%s: %s. Treated as %s, going by %s.", label, joined, state, decider)
}

// DetailLines are the lines `models show` prints.
func DetailLines(m Model, installed, enabled, isDefault bool, catalogPath string) []string {
	c := m.Catalog
	mode := ModeLabels[c.Kind]
	if k, ok := c.Capabilities.(map[string]any); ok && (k["kind"] == "embedding" || k["kind"] == "decision") {
		mode = ModeLabels[k["kind"].(string)]
	}
	if c.QualificationState == "experimental" {
		mode += " (experimental, not yet qualified)"
	}
	status := []string{"Not installed", "Disabled"}
	if installed {
		status[0] = "Installed"
	}
	if enabled {
		status[1] = "Enabled"
	}
	if isDefault {
		status = append(status, "Default")
	}
	cat := TrustLabels[c.Trust] + " · " + TierLabels[c.CatalogTier]
	if catalogPath != "" {
		cat += " (" + catalogPath + ")"
	}
	ctx := "Not applicable"
	if c.ContextWindowTokens != nil {
		ctx = FormatTokens(*c.ContextWindowTokens)
	} else if c.Kind == "llm" {
		ctx = "Not listed in the catalog"
	}
	lines := []string{c.DisplayName,
		"  ID:             " + m.ID,
		"  Type:           " + mode,
		"  Status:         " + strings.Join(status, " · "),
		"  License:        " + LicenseLabels[c.CommercialStatus],
		"  Catalog:        " + cat,
		"  Publisher:      " + c.Provider,
		"  Source:         " + c.Source + " (revision " + c.Revision + ")",
		"  Context length: " + ctx,
		"  Quantization:   " + c.Quantization,
		"  Summary:        " + c.Summary}
	if c.Notes != "" {
		lines = append(lines, "  Notes:          "+c.Notes)
	}
	lines = append(lines, "", "Files")
	for _, a := range c.Artifacts {
		l := "  " + a.Filename + " · " + FormatFileSize(a.SizeBytes)
		if a.Role == "supplementary" {
			l += " · supporting file"
		}
		if a.Source != nil {
			l += " · from " + a.Source.RepositoryURL + " (revision " + a.Source.Revision + ")"
		}
		lines = append(lines, l)
	}
	lines = append(lines, "", "Capabilities")
	if len(c.CapabilityResolution.Capabilities) == 0 {
		lines = append(lines, "  None listed")
	}
	for _, cp := range c.CapabilityResolution.Capabilities {
		lines = append(lines, "  "+CapabilityLabel(cp.ID)+" · "+SourceLabels[cp.Source])
	}
	for _, u := range c.UnsupportedCapabilities {
		lines = append(lines, "  "+CapabilityLabel(u.ID)+" · Verified unsupported: "+u.Reason)
	}
	for _, mm := range c.CapabilityResolution.Mismatches {
		lines = append(lines, "  Note: "+DescribeMismatch(mm))
	}
	mem := c.Memory
	lines = append(lines, "", "Memory", fmt.Sprintf("  Needs at least: %d GB (%s)", mem.MinimumVramEstimateGb, strings.ToLower(MemoryFigureLabels[mem.Source])))
	lines = append(lines, fmt.Sprintf("  %s: %s with %s", MemoryFigureLabels[mem.Reference.Source], FormatGB(mem.Reference.Bytes), DescribeLaunchShape(mem.Reference.LaunchShape)))
	for _, p := range mem.ObservedPeaks {
		day := p.ObservedAt
		if len(day) > 10 {
			day = day[:10]
		}
		lines = append(lines, fmt.Sprintf("  Seen on this machine: %s with %s, %s", FormatGB(p.PeakBytes), DescribeLaunchShape(p.LaunchShape), day))
	}
	lines = append(lines, "  This machine:   "+DescribeFit(mem.Fit.Verdict, mem.Fit.SystemMemoryGb, mem.Fit.HeadroomGb))
	return lines
}
