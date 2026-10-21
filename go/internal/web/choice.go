package web

import (
	"regexp"
	"sort"
	"strings"
)

// Download choice and catalog matching for the discovery page (TS
// model-choice.ts, golden in testdata/choice.json).

// downloadChoice is the part of a discovery choice the ranking reads.
type downloadChoice struct {
	Path      string
	SizeBytes int64
	Quant     *string
	Verdict   string // fits | tight | wont-fit | "" (unknown)
}

var lowQualityRe = regexp.MustCompile(`(?i)(?:^|[-_])(?:IQ[12]|Q[12]|TQ[12])(?:_|$)`)

// lowQualityQuant reports very low-bit quantizations (1–2 bit).
func lowQualityQuant(quant *string) bool {
	return quant != nil && lowQualityRe.MatchString(*quant)
}

var (
	auxiliaryRe = regexp.MustCompile(`(?i)(?:^|/)(?:mtp[-_.]|mmproj[-_.]|projectors?/)`)
	mtpDirRe    = regexp.MustCompile(`(?i)(?:^|/)mtp/`)
	balancedRe  = regexp.MustCompile(`^Q[4568]`)
)

// recommendedChoice prefers balanced Q4 files with known headroom and never
// recommends aggressive low-bit files. It returns -1 when none qualifies.
func recommendedChoice(choices []downloadChoice) int {
	rank := func(c downloadChoice) int {
		quant := ""
		if c.Quant != nil {
			quant = strings.TrimPrefix(strings.ToUpper(*c.Quant), "UD-")
		}
		quality := 3
		switch {
		case quant == "Q4_K_M":
			quality = 0
		case quant == "Q4_K_XL":
			quality = 1
		case balancedRe.MatchString(quant):
			quality = 2
		}
		fit := 2
		switch c.Verdict {
		case "fits":
			fit = 0
		case "tight":
			fit = 1
		}
		return fit*10 + quality
	}
	best := -1
	for i, c := range choices {
		if c.Quant == nil || lowQualityQuant(c.Quant) || auxiliaryRe.MatchString(c.Path) || mtpDirRe.MatchString(c.Path) || c.Verdict == "wont-fit" {
			continue
		}
		// The first of equal ranks wins, as a stable sort's first element does.
		if best == -1 || rank(c) < rank(choices[best]) {
			best = i
		}
	}
	return best
}

// orderedChoices returns indexes: the recommendation first, then by
// increasing download size, then by path.
func orderedChoices(choices []downloadChoice) []int {
	rec := recommendedChoice(choices)
	idx := make([]int, len(choices))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		x, y := choices[idx[a]], choices[idx[b]]
		if (idx[a] == rec) != (idx[b] == rec) {
			return idx[a] == rec
		}
		if x.SizeBytes != y.SizeBytes {
			return x.SizeBytes < y.SizeBytes
		}
		return localeCompare(x.Path, y.Path) < 0
	})
	return idx
}

// localeCompare approximates String.prototype.localeCompare for file paths:
// case-insensitive first, then lowercase before uppercase.
func localeCompare(a, b string) int {
	if c := strings.Compare(strings.ToLower(a), strings.ToLower(b)); c != 0 {
		return c
	}
	return -strings.Compare(a, b)
}

// catalogIdentity is what matching reads from a catalog model.
type catalogIdentity struct {
	ID                 string
	Name               string
	DisplayName        string
	Source             string
	QualificationState string
	Trust              string
}

var (
	identityQuantRe = regexp.MustCompile(`(?i)[-_](?:ud[-_])?(?:i?q\d.*|bf16|f16)$`)
	identityGGUFRe  = regexp.MustCompile(`(?i)[-_]gguf$`)
	identityOtherRe = regexp.MustCompile(`[^a-z0-9]`)
)

func modelIdentity(name string) string {
	s := strings.ToLower(name)
	s = identityQuantRe.ReplaceAllString(s, "")
	s = identityGGUFRe.ReplaceAllString(s, "")
	return identityOtherRe.ReplaceAllString(s, "")
}

// qualifiedCatalogMatch is the qualified catalog model for a Hub repository:
// the same source repository, or exactly the same normalized name. A
// derivative never matches by substring. It returns -1 for none.
func qualifiedCatalogMatch(repo string, models []catalogIdentity) int {
	name := repo
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		name = repo[i+1:]
	}
	for i, m := range models {
		if m.QualificationState != "qualified" || (m.Trust != "" && m.Trust != "qualified") {
			continue
		}
		label := m.DisplayName
		if label == "" {
			label = m.Name
		}
		if strings.TrimSuffix(m.Source, "/") == "https://huggingface.co/"+repo || modelIdentity(label) == modelIdentity(name) {
			return i
		}
	}
	return -1
}
