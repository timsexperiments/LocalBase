package catalog

import (
	"encoding/json"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
)

// Feed annotations (TS catalog/annotations.ts): what the project verified
// about a Hugging Face repository at a pinned revision — status, capability
// marks, measured memory peaks and multi-file recipes — published in the v2
// feed. Readers are tolerant: a bad annotation is skipped and counted, a bad
// field inside a good one is dropped.

const MaxFeedAnnotations = 5_000

var (
	repoRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	revisionRe  = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
	sha256AnyRe = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
	dateRe      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// MemoryPeak is a measured peak at a named launch shape.
type MemoryPeak struct {
	Variant                 string `json:"variant"`
	LaunchShape             string `json:"launchShape"`
	PeakBytes               int64  `json:"peakBytes"`
	BackendAcceleratorBytes int64  `json:"backendAcceleratorBytes"`
	HostBufferBytes         int64  `json:"hostBufferBytes"`
	PeakRSSBytes            int64  `json:"peakRssBytes"`
	SystemDeltaBytes        *int64 `json:"systemDeltaBytes,omitempty"`
	MeasuredAt              string `json:"measuredAt"`
	Hardware                string `json:"hardware"`
}

type RecipeFile struct {
	RepositoryURL string `json:"repositoryUrl"`
	Revision      string `json:"revision"`
	Path          string `json:"path"`
	SizeBytes     int64  `json:"sizeBytes"`
	SHA256        string `json:"sha256"`
	Role          string `json:"role"`
}

type Recipe struct {
	ModelID    string       `json:"modelId"`
	Files      []RecipeFile `json:"files"`
	TotalBytes int64        `json:"totalBytes"`
}

// Annotation is one published annotation, invalid optional fields dropped.
type Annotation struct {
	Repo                    string        `json:"repo"`
	Revision                string        `json:"revision"`
	File                    string        `json:"file,omitempty"`
	ModelID                 string        `json:"modelId,omitempty"`
	Kind                    string        `json:"kind,omitempty"`
	Status                  string        `json:"status,omitempty"`
	UnsupportedCapabilities []Unsupported `json:"unsupportedCapabilities,omitempty"`
	MemoryPeaks             []MemoryPeak  `json:"memoryPeaks,omitempty"`
	Recipe                  *Recipe       `json:"recipe,omitempty"`
}

// CapabilityIDs are the catalog capability ids, CapabilityLabels their names.
var CapabilityIDs = []string{"text-generation", "tool-calling", "reasoning", "embeddings", "decision", "code-generation",
	"code-editing", "code-retrieval", "agentic-coding", "long-context", "multilingual", "translation", "speech-to-text",
	"text-to-speech", "text-to-image", "image-to-image", "text-to-video", "image-to-video", "speech-to-video", "mixture-of-experts"}

var CapabilityLabels = map[string]string{
	"text-generation": "Text generation", "tool-calling": "Tool calling", "reasoning": "Reasoning", "embeddings": "Embeddings",
	"decision": "Decisions", "code-generation": "Code generation", "code-editing": "Code editing", "code-retrieval": "Code retrieval",
	"agentic-coding": "Agentic coding", "long-context": "Long context", "multilingual": "Multilingual", "translation": "Translation",
	"speech-to-text": "Speech to text", "text-to-speech": "Text to speech", "text-to-image": "Text to image",
	"image-to-image": "Image editing", "text-to-video": "Text to video", "image-to-video": "Image to video",
	"speech-to-video": "Speech to video", "mixture-of-experts": "Mixture of experts",
}

func jsLen(s string) int { return len(utf16.Encode([]rune(s))) }

// The value helpers read jsjson values (objects, arrays, float64).
func str(o *jsjson.Object, k string) (string, bool) {
	v, ok := o.Get(k)
	s, isStr := v.(string)
	return s, ok && isStr
}

func strIn(o *jsjson.Object, k string, min, max int) (string, bool) {
	s, ok := str(o, k)
	return s, ok && jsLen(s) >= min && jsLen(s) <= max
}

// safeInt is a JSON number zod's int() accepts (a safe integer).
func safeInt(v any) (int64, bool) {
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || math.Abs(f) > 1<<53-1 {
		return 0, false
	}
	return int64(f), true
}

// strict reports an object whose keys are all in allowed.
func strict(o *jsjson.Object, allowed ...string) bool {
	for _, k := range o.Keys() {
		if !slices.Contains(allowed, k) {
			return false
		}
	}
	return true
}

func parseUnsupported(v any) (Unsupported, bool) {
	o, ok := v.(*jsjson.Object)
	if !ok || !strict(o, "id", "supported", "source", "verified", "reason") {
		return Unsupported{}, false
	}
	id, _ := str(o, "id")
	sup, _ := o.Get("supported")
	src, _ := str(o, "source")
	ver, _ := o.Get("verified")
	reason, okR := strIn(o, "reason", 1, math.MaxInt)
	if !slices.Contains(CapabilityIDs, id) || sup != false || src != "declared" || ver != true || !okR {
		return Unsupported{}, false
	}
	return Unsupported{ID: id, Supported: false, Source: "declared", Verified: true, Reason: reason}, true
}

func nonNeg(o *jsjson.Object, k string) (int64, bool) {
	v, _ := o.Get(k)
	n, ok := safeInt(v)
	return n, ok && n >= 0
}

func parsePeak(v any) (MemoryPeak, bool) {
	o, ok := v.(*jsjson.Object)
	if !ok || !strict(o, "variant", "launchShape", "peakBytes", "backendAcceleratorBytes", "hostBufferBytes", "peakRssBytes",
		"systemDeltaBytes", "measuredAt", "hardware") {
		return MemoryPeak{}, false
	}
	var p MemoryPeak
	var oks [8]bool
	p.Variant, oks[0] = strIn(o, "variant", 1, 100)
	p.LaunchShape, oks[1] = strIn(o, "launchShape", 1, 200)
	p.PeakBytes, oks[2] = nonNeg(o, "peakBytes")
	p.BackendAcceleratorBytes, oks[3] = nonNeg(o, "backendAcceleratorBytes")
	p.HostBufferBytes, oks[4] = nonNeg(o, "hostBufferBytes")
	p.PeakRSSBytes, oks[5] = nonNeg(o, "peakRssBytes")
	p.MeasuredAt, oks[6] = str(o, "measuredAt")
	oks[6] = oks[6] && dateRe.MatchString(p.MeasuredAt)
	p.Hardware, oks[7] = strIn(o, "hardware", 1, 200)
	for _, b := range oks {
		if !b {
			return MemoryPeak{}, false
		}
	}
	if _, present := o.Get("systemDeltaBytes"); present {
		n, ok := nonNeg(o, "systemDeltaBytes")
		if !ok {
			return MemoryPeak{}, false
		}
		p.SystemDeltaBytes = &n
	}
	return p, true
}

// whatwgURL reports a string the WHATWG URL parser accepts (zod's url()).
func whatwgURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "ftp", "ws", "wss":
		return u.Host != ""
	}
	return true
}

func parseRecipe(v any) (*Recipe, bool) {
	o, ok := v.(*jsjson.Object)
	if !ok || !strict(o, "modelId", "files", "totalBytes") {
		return nil, false
	}
	var r Recipe
	r.ModelID, ok = strIn(o, "modelId", 1, 200)
	if !ok {
		return nil, false
	}
	fv, _ := o.Get("files")
	files, ok := fv.([]any)
	if !ok || len(files) < 1 || len(files) > 64 {
		return nil, false
	}
	for _, item := range files {
		f, ok := item.(*jsjson.Object)
		if !ok || !strict(f, "repositoryUrl", "revision", "path", "sizeBytes", "sha256", "role") {
			return nil, false
		}
		var rf RecipeFile
		var oks [6]bool
		rf.RepositoryURL, oks[0] = str(f, "repositoryUrl")
		oks[0] = oks[0] && whatwgURL(rf.RepositoryURL)
		rf.Revision, oks[1] = str(f, "revision")
		oks[1] = oks[1] && revisionRe.MatchString(rf.Revision)
		rf.Path, oks[2] = strIn(f, "path", 1, 500)
		sv, _ := f.Get("sizeBytes")
		rf.SizeBytes, oks[3] = safeInt(sv)
		oks[3] = oks[3] && rf.SizeBytes > 0
		rf.SHA256, oks[4] = str(f, "sha256")
		oks[4] = oks[4] && sha256AnyRe.MatchString(rf.SHA256)
		rf.Role, oks[5] = str(f, "role")
		oks[5] = oks[5] && (rf.Role == "primary" || rf.Role == "supplementary")
		for _, b := range oks {
			if !b {
				return nil, false
			}
		}
		r.Files = append(r.Files, rf)
	}
	tv, _ := o.Get("totalBytes")
	r.TotalBytes, ok = safeInt(tv)
	if !ok || r.TotalBytes <= 0 {
		return nil, false
	}
	return &r, true
}

// validItems is TS validItems: an array of at most 64 (else nothing), keeping
// the items that validate.
func validItems[T any](o *jsjson.Object, k string, parse func(any) (T, bool)) []T {
	v, _ := o.Get(k)
	arr, ok := v.([]any)
	if !ok || len(arr) > 64 {
		return nil
	}
	var out []T
	for _, item := range arr {
		if p, ok := parse(item); ok {
			out = append(out, p)
		}
	}
	return out
}

// ParseAnnotations reads a feed's annotations, skipping the ones that do not
// validate (TS parseFeedAnnotations).
func ParseAnnotations(items []any) (annotations []Annotation, skipped int) {
	annotations = []Annotation{}
	for i, item := range items {
		if i >= MaxFeedAnnotations {
			skipped += len(items) - MaxFeedAnnotations
			break
		}
		o, ok := item.(*jsjson.Object)
		repo, okRepo := "", false
		rev, okRev := "", false
		if ok {
			repo, okRepo = str(o, "repo")
			rev, okRev = str(o, "revision")
		}
		if !ok || !okRepo || !repoRe.MatchString(repo) || !okRev || !revisionRe.MatchString(rev) {
			skipped++
			continue
		}
		a := Annotation{Repo: repo, Revision: rev}
		a.File, _ = strIn(o, "file", 1, 500)
		a.ModelID, _ = strIn(o, "modelId", 1, 200)
		if k, ok := str(o, "kind"); ok && slices.Contains([]string{"llm", "stt", "tts", "image", "video"}, k) {
			a.Kind = k
		}
		if s, ok := str(o, "status"); ok && (s == "qualified" || s == "verified") {
			a.Status = s
		}
		a.UnsupportedCapabilities = validItems(o, "unsupportedCapabilities", parseUnsupported)
		a.MemoryPeaks = validItems(o, "memoryPeaks", parsePeak)
		if rv, present := o.Get("recipe"); present {
			if r, ok := parseRecipe(rv); ok {
				a.Recipe = r
			}
		}
		annotations = append(annotations, a)
	}
	return annotations, skipped
}

// --- lookup and application -------------------------------------------------

// HuggingFaceRepoOf is `org/name` for a Hugging Face model URL, "" otherwise.
func HuggingFaceRepoOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || strings.ToLower(u.Hostname()) != "huggingface.co" || !strings.EqualFold(u.Scheme, "https") && !strings.EqualFold(u.Scheme, "http") {
		return ""
	}
	var parts []string
	for _, p := range strings.Split(u.Path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) < 2 {
		return ""
	}
	repo := parts[0] + "/" + parts[1]
	if !repoRe.MatchString(repo) {
		return ""
	}
	return repo
}

type primaryKey struct{ repo, revision, file string }

func (s *Spec) primaryKey() (primaryKey, bool) {
	for _, a := range s.Artifacts {
		if a.Role != "primary" {
			continue
		}
		repoURL, rev := s.Source, s.RepositoryRevision
		if len(a.Source) > 0 {
			var src struct{ RepositoryURL, Revision string }
			if json.Unmarshal(a.Source, &src) == nil {
				if src.RepositoryURL != "" {
					repoURL = src.RepositoryURL
				}
				if src.Revision != "" {
					rev = src.Revision
				}
			}
		}
		repo := HuggingFaceRepoOf(repoURL)
		if repo == "" {
			return primaryKey{}, false
		}
		return primaryKey{repo, strings.ToLower(rev), a.SourcePath}, true
	}
	return primaryKey{}, false
}

func (c *Catalog) indexAnnotations() {
	c.byRepo = map[string][]int{}
	for i, a := range c.Annotations {
		k := strings.ToLower(a.Repo)
		c.byRepo[k] = append(c.byRepo[k], i)
	}
}

// AnnotationFor is a model's annotation: an exact repo + revision (+ file)
// match first, else one for the same model id and repository at another
// revision (shown, never applied).
func (c *Catalog) AnnotationFor(s *Spec) (a *Annotation, exact bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	key, ok := s.primaryKey()
	if !ok {
		return nil, false
	}
	idx := c.byRepo[strings.ToLower(key.repo)]
	for _, i := range idx {
		x := &c.Annotations[i]
		if strings.ToLower(x.Revision) == key.revision && (x.File == "" || x.File == key.file) {
			return x, true
		}
	}
	for _, i := range idx {
		if x := &c.Annotations[i]; x.ModelID == s.ModelID {
			return x, false
		}
	}
	return nil, false
}

// PlannableFeedPeaks are the measured peaks memory planning may use: only
// from an exact match, and, when it carries a recipe, the same primary sha256.
func (c *Catalog) PlannableFeedPeaks(s *Spec) []MemoryPeak {
	a, exact := c.AnnotationFor(s)
	if a == nil || !exact {
		return nil
	}
	pinned := strings.ToLower(s.Primary().SHA256)
	if a.Recipe != nil {
		for _, f := range a.Recipe.Files {
			if f.Role == "primary" {
				if pinned != "" && !strings.EqualFold(f.SHA256, pinned) {
					return nil
				}
				break
			}
		}
	}
	return a.MemoryPeaks
}

// UserControlled: a user file added or patched the entry.
func (s *Spec) UserControlled() bool {
	return s.Tier == "user" || s.Origin == "user" || s.Trust == "user" || s.Trust == "modified"
}

// AppliedFeedMarks are the feed's capability marks that apply: exact
// matches only, minus marks the spec has, minus capabilities a
// user-controlled entry declares.
func (c *Catalog) AppliedFeedMarks(s *Spec) []Unsupported {
	a, exact := c.AnnotationFor(s)
	if a == nil || !exact {
		return nil
	}
	var out []Unsupported
	for _, m := range a.UnsupportedCapabilities {
		own := slices.ContainsFunc(s.UnsupportedCapabilities, func(u Unsupported) bool { return u.ID == m.ID })
		if own || (s.UserControlled() && s.HasCapability(m.ID)) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// EffectiveUnsupported is the spec's own marks followed by the feed marks
// that apply.
func (c *Catalog) EffectiveUnsupported(s *Spec) []Unsupported {
	feed := c.AppliedFeedMarks(s)
	if len(feed) == 0 {
		return s.UnsupportedCapabilities
	}
	return append(slices.Clone(s.UnsupportedCapabilities), feed...)
}

// AnnotationSummary is what the API and UI show about an annotation.
type AnnotationSummary struct {
	Repo                    string         `json:"repo"`
	Revision                string         `json:"revision"`
	File                    *string        `json:"file"`
	ModelID                 *string        `json:"modelId"`
	Status                  *string        `json:"status"`
	RevisionMatch           bool           `json:"revisionMatch"`
	UnsupportedCapabilities []SummaryMark  `json:"unsupportedCapabilities"`
	MemoryPeaks             []MemoryPeak   `json:"memoryPeaks"`
	Recipe                  *RecipeSummary `json:"recipe"`
}

type SummaryMark struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Reason  string `json:"reason"`
	Applied bool   `json:"applied"`
}

type RecipeSummary struct {
	ModelID    string              `json:"modelId"`
	FileCount  int                 `json:"fileCount"`
	TotalBytes int64               `json:"totalBytes"`
	Files      []RecipeSummaryFile `json:"files"`
}

type RecipeSummaryFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// AnnotationSummaryFor is a model's annotation summary, or nil.
func (c *Catalog) AnnotationSummaryFor(s *Spec) *AnnotationSummary {
	a, exact := c.AnnotationFor(s)
	if a == nil {
		return nil
	}
	applied := map[string]bool{}
	for _, m := range c.AppliedFeedMarks(s) {
		applied[m.ID] = true
	}
	out := &AnnotationSummary{Repo: a.Repo, Revision: a.Revision, File: optional(a.File), ModelID: optional(a.ModelID),
		Status: optional(a.Status), RevisionMatch: exact, UnsupportedCapabilities: []SummaryMark{}, MemoryPeaks: a.MemoryPeaks}
	if out.MemoryPeaks == nil {
		out.MemoryPeaks = []MemoryPeak{}
	}
	for _, m := range a.UnsupportedCapabilities {
		label := CapabilityLabels[m.ID]
		if label == "" {
			label = m.ID
		}
		out.UnsupportedCapabilities = append(out.UnsupportedCapabilities, SummaryMark{m.ID, label, m.Reason, applied[m.ID]})
	}
	if r := a.Recipe; r != nil {
		rs := &RecipeSummary{ModelID: r.ModelID, FileCount: len(r.Files), TotalBytes: r.TotalBytes}
		for _, f := range r.Files {
			rs.Files = append(rs.Files, RecipeSummaryFile{f.Path, f.SizeBytes})
		}
		out.Recipe = rs
	}
	return out
}
