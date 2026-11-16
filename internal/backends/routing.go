package backends

import (
	"slices"
	"sort"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/models"
)

// routing is the versioned rule table (routing.json): per platform, the
// order in which automatic selection tries backends. An entry is a backend
// id or "engine:<name>" for any backend of that engine (so a registered MLX
// backend can be preferred on Apple Silicon). Backends no rule names are
// never chosen automatically, only by hand.
type routing struct {
	Schema string `json:"schema"`
	Rules  []struct {
		OS     string   `json:"os"`
		Arch   string   `json:"arch"`
		Prefer []string `json:"prefer"`
	} `json:"rules"`
}

// Preference returns the backend ids automatic selection tries on host h, in
// order.
func (r *Registry) Preference(h Host) []string {
	var prefer []string
	r.mu.RLock()
	rules := r.routing.Rules
	r.mu.RUnlock()
	for _, rule := range rules {
		if (rule.OS == h.OS || rule.OS == "*") && (rule.Arch == h.Arch || rule.Arch == "*") {
			prefer = rule.Prefer
			break
		}
	}
	var out []string
	for _, p := range prefer {
		if engine, ok := strings.CutPrefix(p, "engine:"); ok {
			for _, d := range r.All() {
				if d.Engine == engine && !slices.Contains(out, d.ID) {
					out = append(out, d.ID)
				}
			}
			continue
		}
		if r.Get(p) != nil && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// Recommendation is the automatic choice and every option with its finding.
type Recommendation struct {
	// BackendID and VariantID are empty when nothing plausible was found.
	BackendID string `json:"backendId"`
	VariantID string `json:"variantId"`
	// Options lists every registered backend: recommended first, then other
	// plausible ones, then the rest. Each is selectable.
	Options []Finding `json:"options"`
}

// Recommend applies the routing rules to model m. inspections maps variant
// ids to their file inspection (for missing files); a variant without one
// is rated on its record alone.
func (r *Registry) Recommend(m *models.Model, h Host, inspections map[string]*models.Inspection) Recommendation {
	assess := func(d *Definition, v *models.Variant) Finding {
		var missing []string
		vv := v
		if in := inspections[v.ID]; in != nil {
			missing, vv = in.Missing, &in.Variant
		}
		return Assess(m, vv, d, h, missing)
	}
	rec := Recommendation{}
	for _, id := range r.Preference(h) {
		d := r.Get(id)
		if h.Install[id] == Missing || h.Unpinned[id] || !d.automaticFor(m) {
			continue // automatic selection only picks something that can run here and may be picked
		}
		for i := range m.Variants {
			f := assess(d, &m.Variants[i])
			if Plausible(f.Rating) && len(f.Drops) == 0 {
				rec.BackendID, rec.VariantID = id, f.VariantID
				break
			}
		}
		if rec.BackendID != "" {
			break
		}
	}
	// Every backend appears once, with its best variant.
	for _, d := range r.All() {
		var best *Finding
		for i := range m.Variants {
			f := assess(d, &m.Variants[i])
			if d.ID == rec.BackendID && f.VariantID != rec.VariantID {
				continue
			}
			if best == nil || better(f, *best) {
				best = &f
			}
		}
		rec.Options = append(rec.Options, *best)
	}
	rank := map[string]int{}
	for i, id := range r.Preference(h) {
		rank[id] = i
	}
	order := func(f Finding) int {
		if f.BackendID == rec.BackendID {
			return -1
		}
		if i, ok := rank[f.BackendID]; ok {
			return i
		}
		return len(rank)
	}
	sort.SliceStable(rec.Options, func(i, j int) bool {
		a, b := rec.Options[i], rec.Options[j]
		if ra, rb := ratingRank[a.Rating], ratingRank[b.Rating]; ra != rb && a.BackendID != rec.BackendID && b.BackendID != rec.BackendID {
			return ra > rb
		}
		return order(a) < order(b)
	})
	return rec
}

func better(a, b Finding) bool {
	if ra, rb := ratingRank[a.Rating], ratingRank[b.Rating]; ra != rb {
		return ra > rb
	}
	return len(a.Missing) < len(b.Missing)
}
