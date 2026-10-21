package hub

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// Model discovery (TS models/discovery.ts): search results with the files
// `models add` would pin and how each fits this machine, and the add step.
// The CLI and the gateway share it, so both write the same entries.

const (
	maxChoices        = 40
	previewConcurrent = 4
	maxCatalogNotes   = 3
)

// CatalogOwner is the tier that provides modelID ("" when none).
func CatalogOwner(cat *catalog.Catalog, modelID string) string {
	if s := cat.ByID(modelID); s != nil {
		return s.Origin
	}
	return ""
}

// Fit is a discovery choice's memory verdict.
type Fit struct {
	Verdict   string `json:"verdict"`
	MinVramGB int    `json:"minVramGb"`
	Message   string `json:"message"`
}

// Choice is one file a repository could be added with.
type Choice struct {
	Path       string                     `json:"path"`
	SizeBytes  int64                      `json:"sizeBytes"`
	PartCount  int                        `json:"partCount"`
	Quant      *string                    `json:"quant"`
	ModelID    string                     `json:"modelId"`
	Existing   *string                    `json:"existing"`
	Fit        *Fit                       `json:"fit"`
	Annotation *catalog.AnnotationSummary `json:"annotation"`
}

// Preview is what adding a repository would pin.
type Preview struct {
	Kind        string   `json:"kind"`
	Choices     []Choice `json:"choices"`
	DefaultPath *string  `json:"defaultPath"`
}

// CatalogNote is a feed annotation shown beside a search result.
type CatalogNote struct {
	*catalog.AnnotationSummary
	DisplayName *string `json:"displayName"`
	Installable bool    `json:"installable"`
}

func sameRevision(a catalog.Annotation, sha string) bool {
	return sha != "" && strings.EqualFold(a.Revision, sha)
}

// annotationForFile is the feed annotation for one file of a repository:
// the current revision's first, else one for another revision.
func annotationForFile(cat *catalog.Catalog, repo, sha, path string) (*catalog.Annotation, bool) {
	var candidates []catalog.Annotation
	for _, a := range cat.AnnotationsForRepo(repo) {
		if a.File == "" || a.File == path {
			candidates = append(candidates, a)
		}
	}
	for i := range candidates {
		if sameRevision(candidates[i], sha) {
			return &candidates[i], true
		}
	}
	if len(candidates) > 0 {
		return &candidates[0], false
	}
	return nil, false
}

func choiceAnnotation(cat *catalog.Catalog, info *ModelInfo, path string) *catalog.AnnotationSummary {
	a, match := annotationForFile(cat, info.ID, info.SHA, path)
	if a == nil {
		return nil
	}
	applied := map[string]bool{}
	if match {
		for _, m := range a.UnsupportedCapabilities {
			applied[m.ID] = true
		}
	}
	return catalog.SummarizeAnnotation(a, match, applied)
}

// CatalogNotes are the feed annotations shown beside a search result,
// recipes first.
func CatalogNotes(cat *catalog.Catalog, repo, sha string) []CatalogNote {
	anns := cat.AnnotationsForRepo(repo)
	rank := func(a catalog.Annotation) int {
		r := 2
		if a.Recipe != nil {
			r = 0
		}
		if a.Status == "" {
			r++
		}
		return r
	}
	sort.SliceStable(anns, func(i, j int) bool { return rank(anns[i]) < rank(anns[j]) })
	if len(anns) > maxCatalogNotes {
		anns = anns[:maxCatalogNotes]
	}
	out := []CatalogNote{}
	for i := range anns {
		a := &anns[i]
		note := CatalogNote{AnnotationSummary: catalog.SummarizeAnnotation(a, sameRevision(*a, sha), nil)}
		if a.ModelID != "" {
			if s := cat.ByID(a.ModelID); s != nil && s.DisplayName != "" {
				name := s.DisplayName
				note.DisplayName = &name
			}
		}
		note.Installable = a.Recipe != nil && cat.ByID(a.Recipe.ModelID) != nil
		out = append(out, note)
	}
	return out
}

// WithFeedMarks writes the feed's verified-unsupported marks into a
// generated entry, removing the marked ids from its capabilities.
func WithFeedMarks(spec *jsjson.Object, marks []catalog.Unsupported) *jsjson.Object {
	if len(marks) == 0 {
		return spec
	}
	ids := map[string]bool{}
	for _, m := range marks {
		ids[m.ID] = true
	}
	out := copyObject(spec)
	if caps, ok := spec.Get("capabilities"); ok {
		if arr, ok := caps.([]any); ok {
			var kept []any
			for _, c := range arr {
				id, _ := c.(string)
				if o, ok := c.(*jsjson.Object); ok {
					v, _ := o.Get("id")
					id, _ = v.(string)
				}
				if !ids[id] {
					kept = append(kept, c)
				}
			}
			if kept == nil {
				kept = []any{}
			}
			out.Set("capabilities", kept)
		}
	}
	var list []any
	for _, m := range marks {
		list = append(list, jsjson.FromGo(m))
	}
	out.Set("unsupportedCapabilities", list)
	return out
}

// fitOf evaluates a generated entry like TS evaluateModelFit.
func fitOf(cat *catalog.Catalog, spec *jsjson.Object, systemGB int) *Fit {
	s, err := catalog.DecodeSpec(jsjson.Stringify(spec))
	if err != nil {
		return nil
	}
	// Reference measurements follow the id's catalog provenance, as TS
	// usesReferenceMeasurements does.
	if existing := cat.ByID(s.ModelID); existing != nil {
		s.Trust, s.Origin, s.Tier = existing.Trust, existing.Origin, existing.Tier
	}
	f := runtime.FitFor(s, systemGB, nil, nil, cat.PlannableFeedPeaks(s))
	out := &Fit{Verdict: f.Fit, MinVramGB: f.MinVramGB}
	if f.Fit == "wont-fit" {
		out.Message = fmt.Sprintf("Requires %dGB (%s), you have %dGB", f.MinVramGB, f.Source, systemGB)
	} else {
		out.Message = fmt.Sprintf("Leaves %sGB headroom (%s)", jsjson.ToFixed1(float64(systemGB-f.MinVramGB)), f.Source)
	}
	return out
}

// downloadBytes is every file the entry downloads, shared encoders included.
func downloadBytes(e *Entry) int64 {
	var weights, pinned int64
	for _, w := range e.Weights {
		weights += w.Size
	}
	if arts, ok := e.Spec.Get("artifacts"); ok {
		for _, a := range arts.([]any) {
			if o, ok := a.(*jsjson.Object); ok {
				if v, ok := o.Get("expectedSizeBytes"); ok {
					f, _ := v.(float64)
					pinned += int64(f)
				}
			}
		}
	}
	return max(weights, pinned)
}

func previewFor(cat *catalog.Catalog, info *ModelInfo, systemGB int) (*Preview, error) {
	available, err := EntryChoices(info)
	if err != nil {
		return nil, err
	}
	files := available.Files
	if len(files) > maxChoices {
		files = files[:maxChoices]
	}
	choices := []Choice{}
	for _, f := range files {
		e, err := GenerateEntry(cat, info, Reference{Repo: info.ID, Selector: f.Path}, "")
		if err != nil {
			continue
		}
		c := Choice{Path: f.Path, SizeBytes: downloadBytes(e), PartCount: len(e.Weights), Quant: strPtr(QuantOf(f.Path)),
			ModelID: e.ModelID, Existing: strPtr(CatalogOwner(cat, e.ModelID)), Fit: fitOf(cat, e.Spec, systemGB),
			Annotation: choiceAnnotation(cat, info, f.Path)}
		choices = append(choices, c)
	}
	if len(choices) == 0 {
		return nil, &Error{Message: info.ID + " has no files models add can use."}
	}
	addable := func(c *Choice) bool { return c.Existing == nil || *c.Existing == "user" }
	var preferred, fallback *Choice
	if available.Default != nil {
		for i := range choices {
			if choices[i].Path == available.Default.Path {
				preferred = &choices[i]
				break
			}
		}
	}
	if len(choices) == 1 || available.Default != nil {
		for i := range choices {
			if addable(&choices[i]) {
				fallback = &choices[i]
				break
			}
		}
	}
	picked := fallback
	if preferred != nil && addable(preferred) {
		picked = preferred
	}
	p := &Preview{Kind: available.Kind, Choices: choices}
	if picked != nil {
		p.DefaultPath = &picked.Path
	}
	return p, nil
}

// DiscoveryHit is a search result with its preview.
type DiscoveryHit struct {
	Hit
	URL        string        `json:"url"`
	Preview    *Preview      `json:"preview"`
	Reason     *string       `json:"reason"`
	ReasonCode string        `json:"reasonCode,omitempty"`
	Catalog    []CatalogNote `json:"catalog"`
}

// Discover searches the Hub and previews the most downloaded addable
// results (at most previewLimit).
func Discover(ctx context.Context, c *Client, cat *catalog.Catalog, kind, query string, systemGB, limit, previewLimit int) ([]DiscoveryHit, bool, error) {
	if limit <= 0 {
		limit = 20
	}
	if previewLimit <= 0 {
		previewLimit = 12
	}
	hits, cached, err := c.Search(ctx, kind, query, limit)
	if err != nil {
		return nil, false, err
	}
	out := make([]DiscoveryHit, len(hits))
	// Previews are taken in result order, as TS hands them out.
	previewed := make([]bool, len(hits))
	budget := previewLimit
	for i, h := range hits {
		if h.Likely.Kind != nil && budget > 0 {
			previewed[i] = true
			budget--
		}
	}
	sem := make(chan struct{}, previewConcurrent)
	done := make(chan struct{}, len(hits))
	for i, h := range hits {
		go func(i int, h Hit) {
			defer func() { done <- struct{}{} }()
			d := DiscoveryHit{Hit: h, URL: RepoURL(h.Repo), Catalog: CatalogNotes(cat, h.Repo, h.SHA)}
			switch {
			case h.Likely.Kind == nil:
				label := h.Likely.Label
				d.Reason = &label
			case previewed[i]:
				sem <- struct{}{}
				info, err := c.ModelInfo(ctx, h.Repo)
				if err == nil {
					d.Catalog = CatalogNotes(cat, h.Repo, info.SHA)
					d.Preview, err = previewFor(cat, info, systemGB)
				}
				<-sem
				if err != nil {
					d.Preview = nil
					msg := "Could not read this repository from Hugging Face."
					if he, ok := err.(*Error); ok {
						msg, d.ReasonCode = he.Message, he.Code
					}
					d.Reason = &msg
				}
			}
			out[i] = d
		}(i, h)
	}
	for range hits {
		<-done
	}
	return out, cached, nil
}

// PreviewRepository previews one repository.
func PreviewRepository(ctx context.Context, c *Client, cat *catalog.Catalog, repo string, systemGB int) (*Preview, error) {
	info, err := c.ModelInfo(ctx, repo)
	if err != nil {
		return nil, err
	}
	return previewFor(cat, info, systemGB)
}

// ConflictError: a built-in or remote entry already owns the id.
type ConflictError struct{ Message string }

func (e *ConflictError) Error() string { return e.Message }

// Added is a pinned repository file written to the user catalog.
type Added struct {
	Entry    *Entry
	Path     string
	Model    *catalog.Spec
	Replaced bool
}

// AddFromHub pins one repository file as a user catalog entry, with the
// feed's marks when its annotation matches the revision. It refuses ids a
// built-in or remote entry owns and replaces a user entry with the same id.
func AddFromHub(ctx context.Context, c *Client, cat *catalog.Catalog, repo, selector, modelID string) (*Added, error) {
	conflict := func(id string) (bool, error) {
		owner := CatalogOwner(cat, id)
		if owner != "" && owner != "user" {
			return false, &ConflictError{fmt.Sprintf("%q is already a %s catalog model. Choose another id.", id, owner)}
		}
		return owner == "user", nil
	}
	if modelID != "" {
		if _, err := conflict(modelID); err != nil {
			return nil, err
		}
	}
	info, err := c.ModelInfo(ctx, repo)
	if err != nil {
		return nil, err
	}
	e, err := GenerateEntry(cat, info, Reference{Repo: repo, Selector: selector}, modelID)
	if err != nil {
		return nil, err
	}
	replaced, err := conflict(e.ModelID)
	if err != nil {
		return nil, err
	}
	spec := e.Spec
	if len(e.Weights) > 0 {
		if a, match := annotationForFile(cat, info.ID, info.SHA, e.Weights[0].Path); a != nil && match {
			spec = WithFeedMarks(spec, a.UnsupportedCapabilities)
		}
	}
	path, model, err := catalog.WriteUserEntry(c.Root, spec)
	if err != nil {
		return nil, err
	}
	return &Added{Entry: e, Path: path, Model: model, Replaced: replaced}, nil
}
