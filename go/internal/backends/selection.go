package backends

import (
	"github.com/timsexperiments/LocalBase/go/internal/models"
)

// Modes of a selection.
const (
	ModeAuto   = "auto"
	ModeManual = "manual"
)

// Choice is the stored, per-device selection. Automatic mode stores nothing;
// a manual choice keeps its backend id even if that backend disappears.
type Choice struct {
	Mode      string `json:"mode"`
	BackendID string `json:"backendId"`
	VariantID string `json:"variantId,omitempty"`
}

// Selection is what runs a model on this device right now.
type Selection struct {
	Mode      string `json:"mode"`
	BackendID string `json:"backendId"` // empty: no backend selected
	VariantID string `json:"variantId"`
	// Unavailable: a manual choice names a backend that is no longer
	// registered. Nothing is substituted; the person picks a replacement.
	Unavailable bool `json:"unavailable"`
	// Finding rates the selected backend and variant (nil when none).
	Finding *Finding `json:"finding"`
	// NeedsDownload: the selected variant's files that are not present, and
	// their known size; selecting another engine never swaps files silently.
	NeedsDownload []string `json:"needsDownload,omitempty"`
	DownloadBytes int64    `json:"downloadBytes,omitempty"`
	// Recommendation is the automatic result, shown even in manual mode.
	Recommendation Recommendation `json:"recommendation"`
}

// Resolve combines the stored choice with fresh detection.
func (r *Registry) Resolve(m *models.Model, h Host, inspections map[string]*models.Inspection, c *Choice) Selection {
	rec := r.Recommend(m, h, inspections)
	s := Selection{Mode: ModeAuto, Recommendation: rec}
	if c == nil || c.Mode != ModeManual {
		s.BackendID, s.VariantID = rec.BackendID, rec.VariantID
	} else {
		s.Mode, s.BackendID, s.VariantID = ModeManual, c.BackendID, c.VariantID
		if r.Get(c.BackendID) == nil {
			s.Unavailable = true
			return s
		}
		if s.VariantID == "" || m.Variant(s.VariantID) == nil {
			// The best-rated variant for the chosen backend.
			for _, f := range rec.Options {
				if f.BackendID == c.BackendID {
					s.VariantID = f.VariantID
				}
			}
		}
	}
	if s.BackendID == "" {
		return s
	}
	v := m.Variant(s.VariantID)
	if v == nil {
		return s
	}
	var missing []string
	vv := v
	if in := inspections[v.ID]; in != nil {
		missing, vv = in.Missing, &in.Variant
	}
	f := Assess(m, vv, r.Get(s.BackendID), h, missing)
	s.Finding = &f
	if len(missing) > 0 {
		s.NeedsDownload = missing
		for _, a := range vv.Artifacts {
			for _, name := range missing {
				if a.Filename == name {
					s.DownloadBytes += a.SizeBytes
				}
			}
		}
	}
	return s
}
