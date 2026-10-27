package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/hub"
	"github.com/timsexperiments/LocalBase/go/internal/meta"
)

// Get models (§15 step 5): Hugging Face discovery over
// /_localbase/model-discovery, as the signed-in person (TS
// model-discovery.tsx, following the Shelf brief D01–D04). Search is a GET
// with the query in the URL; the card actions are htmx POSTs that redraw
// the part of the card they change.

var discoveryKinds = []struct{ Value, Label string }{{"llm", "Chat"}, {"stt", "Transcription"}, {"image", "Image"}}

var fitLabels = map[string]string{"fits": "Fits", "tight": "Tight fit", "wont-fit": "Won't fit"}

// tokenCodes are the errors a saved Hugging Face token can fix.
var tokenCodes = map[string]bool{"hub_auth_required": true, "hub_rate_limited": true}

// discoveryFailure is an API error with whether a token would help.
type discoveryFailure struct {
	Message string
	Token   bool
}

func (u *UI) discovery(r *http.Request, body map[string]any, out any) *discoveryFailure {
	b, _ := json.Marshal(body)
	res := u.call(r, "POST", "/_localbase/model-discovery", "application/json", b)
	if res.Status != 200 {
		var e struct {
			Error struct{ Message, Code string } `json:"error"`
		}
		json.Unmarshal(res.Body, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = "Request failed (" + strconv.Itoa(res.Status) + "). Try again."
		}
		return &discoveryFailure{Message: msg, Token: tokenCodes[e.Error.Code]}
	}
	if json.Unmarshal(res.Body, out) != nil {
		return &discoveryFailure{Message: "The gateway returned an unexpected response."}
	}
	return nil
}

// choiceView is one download file on a card.
type choiceView struct {
	hub.Choice
	Key         string // unique within the page
	Recommended bool
	Selected    bool
	Addable     bool
	Low         bool
	QuantLabel  string
	Size        string
	FitLabel    string
	Badge       string // the header fit badge
	Memory      string
	Advice      string
	Peak        string
	PeakWhere   string
}

// filesView is the download part of a card.
type filesView struct {
	Repo      string
	Card      string // the card's element id
	Choices   []choiceView
	Selected  *choiceView
	Open      bool
	CanManage bool
}

// cardView is one search result.
type cardView struct {
	Hit       hub.DiscoveryHit
	ID        string
	Org, Name string
	Downloads string
	Status    string // feed status badge text
	Fit       string
	FitHint   string
	Match     *catalogIdentity
	Files     *filesView
	Reason    string
	TokenHint bool
	CanShow   bool // "Show files" applies
	Recipes   []recipeView
	CanManage bool
}

type recipeView struct {
	ModelID, Label, Facts, File string
	Installable                 bool
}

func compactCount(v int64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", float64(v)/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.1fk", float64(v)/1e3)
	}
	return strconv.FormatInt(v, 10)
}

// discoverySize is TS size(): GiB above 1 GiB, else MiB.
func discoverySize(v int64) string {
	if v >= 1<<30 {
		return fmt.Sprintf("%.1f GiB", float64(v)/(1<<30))
	}
	return fmt.Sprintf("%d MiB", max(1, int64(float64(v)/(1<<20)+0.5)))
}

func (u *UI) catalogIdentities() []catalogIdentity {
	var out []catalogIdentity
	for _, s := range u.d.Catalog.All() {
		out = append(out, catalogIdentity{ID: s.ModelID, Name: s.Family, DisplayName: s.Name(), Source: s.Source, QualificationState: s.QualificationState, Trust: s.Trust})
	}
	return out
}

func headlinePeak(a *catalog.AnnotationSummary) *catalog.MemoryPeak {
	var best *catalog.MemoryPeak
	for i := range a.MemoryPeaks {
		if best == nil || a.MemoryPeaks[i].PeakBytes > best.PeakBytes {
			best = &a.MemoryPeaks[i]
		}
	}
	return best
}

func describePeak(p *catalog.MemoryPeak) string {
	shape := meta.DescribeLaunchShape(p.LaunchShape)
	if shape == p.LaunchShape {
		shape = strings.Join(strings.Split(p.LaunchShape, ";"), ", ")
	}
	return meta.FormatGB(p.PeakBytes) + " with " + shape
}

func feedStatus(a *catalog.AnnotationSummary, showRevision bool) string {
	if a == nil || a.Status == nil {
		return ""
	}
	label := map[string]string{"qualified": "Qualified by LocalBase", "verified": "Verified by LocalBase"}[*a.Status]
	if !a.RevisionMatch && showRevision {
		label += " (other revision)"
	}
	return label
}

// files builds the download part of a card from a preview (TS DiscoveryCard).
func files(card string, p *hub.Preview, match bool, canManage bool) *filesView {
	choices := make([]downloadChoice, len(p.Choices))
	for i, c := range p.Choices {
		choices[i] = downloadChoice{Path: c.Path, SizeBytes: c.SizeBytes, Quant: c.Quant}
		if c.Fit != nil {
			choices[i].Verdict = c.Fit.Verdict
		}
	}
	order := orderedChoices(choices)
	rec := recommendedChoice(choices)
	// TS initialPath: the recommendation, else an unidentified file that fits.
	selected := rec
	if selected < 0 {
		for _, i := range order {
			if choices[i].Quant == nil && choices[i].Verdict != "wont-fit" {
				selected = i
				break
			}
		}
	}
	f := &filesView{Repo: "", Card: card, Open: !match, CanManage: canManage}
	for n, i := range order {
		c := p.Choices[i]
		v := choiceView{Choice: c, Key: fmt.Sprintf("%s-f%d", card, n), Recommended: i == rec, Selected: i == selected,
			Addable: c.Existing == nil || *c.Existing == "user", Low: lowQualityQuant(c.Quant), Size: discoverySize(c.SizeBytes), QuantLabel: "Quantization unknown"}
		quant := ""
		if c.Quant != nil {
			quant = *c.Quant
			v.QuantLabel = quant
		}
		switch {
		case v.Low:
			v.FitLabel = "Low quality: prefer Q4 or a smaller model"
			v.Advice = "Very low-bit compression saves space but can noticeably reduce quality. Choose a Q4 file or a smaller model when possible."
		case strings.Contains(strings.ToUpper(quant), "Q4"):
			v.Advice = "A balance of download size and quality. Higher-bit files use more memory for potentially better fidelity."
		case quant != "":
			v.Advice = "Higher-bit files use more memory. Compare the estimated fit before installing."
		default:
			v.Advice = "Quantization is not identified. Check the publisher’s notes and estimated fit before installing."
		}
		if !v.Low {
			v.FitLabel = "Fit unknown"
			if c.Fit != nil {
				v.FitLabel = fitLabels[c.Fit.Verdict]
			}
		}
		if c.Fit != nil {
			v.Badge, v.Memory = fitLabels[c.Fit.Verdict], fmt.Sprintf("~%.1f GB", float64(c.Fit.MinVramGB))
		}
		if c.Annotation != nil {
			if peak := headlinePeak(c.Annotation); peak != nil {
				v.Peak, v.PeakWhere = describePeak(peak), peak.Hardware
			}
		}
		f.Choices = append(f.Choices, v)
	}
	for i := range f.Choices {
		if f.Choices[i].Selected {
			f.Selected = &f.Choices[i]
		}
	}
	return f
}

func (u *UI) card(n int, h hub.DiscoveryHit, idents []catalogIdentity, canManage bool) cardView {
	c := cardView{Hit: h, ID: "hit-" + strconv.Itoa(n), Downloads: compactCount(h.Downloads), CanManage: canManage}
	c.Org, c.Name, _ = strings.Cut(h.Repo, "/")
	if i := qualifiedCatalogMatch(h.Repo, idents); i >= 0 {
		c.Match = &idents[i]
	}
	if h.Preview != nil {
		c.Files = files(c.ID, h.Preview, c.Match != nil, canManage)
		c.Files.Repo = h.Repo
	}
	var annotation *catalog.AnnotationSummary
	if c.Files != nil && c.Files.Selected != nil {
		annotation = c.Files.Selected.Annotation
		if fit := c.Files.Selected.Fit; fit != nil {
			c.Fit, c.FitHint = fitLabels[fit.Verdict], fit.Message
		}
	}
	switch {
	case annotation != nil:
		c.Status = feedStatus(annotation, true)
	case len(h.Catalog) > 0:
		c.Status = feedStatus(h.Catalog[0].AnnotationSummary, h.SHA != "")
	}
	if h.Preview == nil {
		if h.Reason != nil && *h.Reason != h.Likely.Label {
			c.Reason, c.TokenHint = *h.Reason, tokenCodes[h.ReasonCode]
		}
		c.CanShow = h.Likely.Kind != nil && (h.Reason == nil || strings.HasPrefix(*h.Reason, "Could not"))
	}
	for _, note := range h.Catalog {
		if note.Recipe == nil {
			continue
		}
		r := recipeView{ModelID: note.Recipe.ModelID, Label: note.Recipe.ModelID, Installable: note.Installable}
		if note.DisplayName != nil {
			r.Label = *note.DisplayName
		}
		total := note.Recipe.TotalBytes
		r.Facts = fmt.Sprintf("%d files · %s", note.Recipe.FileCount, meta.FormatFileSize(&total))
		if peak := headlinePeak(note.AnnotationSummary); peak != nil {
			r.Facts += " · peak " + describePeak(peak)
		}
		if note.File != nil {
			r.File = *note.File
		}
		c.Recipes = append(c.Recipes, r)
	}
	return c
}

func (u *UI) getModels(w http.ResponseWriter, r *http.Request) {
	p := u.caller(r)
	canManage := p.Can("models:manage")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	kind := r.URL.Query().Get("kind")
	if kind != "stt" && kind != "image" {
		kind = "llm"
	}
	data := map[string]any{"Query": q, "Kind": kind, "Kinds": discoveryKinds, "CanManage": canManage}
	if q == "" {
		filter := taskFilter(r)
		data["Catalog"], data["Tasks"] = u.catalogRows(filter)
		data["Filter"] = filter
	}
	for _, k := range discoveryKinds {
		if k.Value == kind {
			data["KindLabel"] = strings.ToLower(k.Label)
		}
	}
	if q != "" && canManage {
		var res struct {
			Cached bool               `json:"cached"`
			Hits   []hub.DiscoveryHit `json:"hits"`
		}
		if f := u.discovery(r, map[string]any{"action": "search", "query": q, "kind": kind}, &res); f != nil {
			data["Failure"] = f
		} else {
			idents := u.catalogIdentities()
			cards := make([]cardView, 0, len(res.Hits))
			for i, h := range res.Hits {
				cards = append(cards, u.card(i, h, idents, canManage))
			}
			data["Cards"], data["Cached"], data["Searched"] = cards, res.Cached, true
		}
	}
	u.render(w, r, "get", data)
}

// discoverPreview reads a repository's files ("Show files").
func (u *UI) discoverPreview(w http.ResponseWriter, r *http.Request) {
	repo, card := r.PostFormValue("repo"), safeID(r.PostFormValue("card"))
	var res struct {
		Preview *hub.Preview `json:"preview"`
	}
	data := map[string]any{"Card": card, "Repo": repo}
	if f := u.discovery(r, map[string]any{"action": "preview", "repo": repo}, &res); f != nil || res.Preview == nil {
		if f == nil {
			f = &discoveryFailure{Message: "Could not read the repo."}
		}
		data["Failure"] = f
		u.fragment(w, r, "get", "discover-preview-failed", data)
		return
	}
	match := qualifiedCatalogMatch(repo, u.catalogIdentities()) >= 0
	f := files(card, res.Preview, match, true)
	f.Repo = repo
	u.fragment(w, r, "get", "discover-files", f)
}

// discoverAdd saves the chosen file to the catalog, and installs it on request.
func (u *UI) discoverAdd(w http.ResponseWriter, r *http.Request) {
	repo, selector := r.PostFormValue("repo"), r.PostFormValue("selector")
	install := r.PostFormValue("install") == "1"
	data := map[string]any{}
	if selector == "" {
		data["Failure"] = &discoveryFailure{Message: "Choose a file to add."}
		u.fragment(w, r, "get", "discover-result", data)
		return
	}
	var res struct {
		ModelID     string `json:"modelId"`
		DisplayName string `json:"displayName"`
		Replaced    bool   `json:"replaced"`
		Operation   *struct {
			State, Detail string
		} `json:"operation"`
	}
	if f := u.discovery(r, map[string]any{"action": "add", "repo": repo, "selector": selector, "install": install}, &res); f != nil {
		data["Failure"] = f
		u.fragment(w, r, "get", "discover-result", data)
		return
	}
	failed := res.Operation != nil && res.Operation.State == "failed"
	verb := "Added"
	if res.Replaced {
		verb = "Updated"
	}
	notice := fmt.Sprintf("%s %s (%s)", verb, res.DisplayName, res.ModelID)
	if install && !failed {
		notice += "; installing now"
	}
	data["Notice"], data["ModelID"] = notice+".", res.ModelID
	if failed {
		detail := res.Operation.Detail
		if detail == "" {
			detail = "unknown error"
		}
		data["Failure"] = &discoveryFailure{Message: "Added, but the install did not start: " + detail}
	}
	u.fragment(w, r, "get", "discover-result", data)
}

// discoverRecipe installs a catalog-feed recipe.
func (u *UI) discoverRecipe(w http.ResponseWriter, r *http.Request) {
	var res struct {
		ModelID     string `json:"modelId"`
		DisplayName string `json:"displayName"`
		Operation   *struct {
			State, Detail string
		} `json:"operation"`
	}
	data := map[string]any{}
	if f := u.discovery(r, map[string]any{"action": "install-recipe", "modelId": r.PostFormValue("modelId")}, &res); f != nil {
		data["Failure"] = f
	} else if res.Operation != nil && res.Operation.State == "failed" {
		detail := res.Operation.Detail
		if detail == "" {
			detail = "unknown error"
		}
		data["Failure"] = &discoveryFailure{Message: "The install did not start: " + detail}
	} else {
		data["Notice"], data["ModelID"] = fmt.Sprintf("Installing %s (%s).", res.DisplayName, res.ModelID), res.ModelID
	}
	// A catalog row has room for a word, not a sentence.
	if r.PostFormValue("compact") == "1" {
		u.fragment(w, r, "get", "catalog-installing", data)
		return
	}
	u.fragment(w, r, "get", "discover-result", data)
}

// fragment renders one named template of a page.
func (u *UI) fragment(w http.ResponseWriter, r *http.Request, page, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := u.pages[page].ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// safeID keeps an element id the page made (letters, digits, '-').
func safeID(s string) string {
	for _, c := range s {
		if !(c == '-' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return "card"
		}
	}
	if s == "" || len(s) > 40 {
		return "card"
	}
	return s
}
