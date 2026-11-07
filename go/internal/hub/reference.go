package hub

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Reference is `hf:<org>/<repo>[:<file-or-quant>]`.
type Reference struct {
	Repo     string `json:"repo"`
	Selector string `json:"selector,omitempty"`
}

var referenceRe = regexp.MustCompile(`^hf:([^:]+)(?::(.+))?$`)

// ParseReference parses an hf: reference.
func ParseReference(value string) (Reference, error) {
	m := referenceRe.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return Reference{}, &Error{Message: fmt.Sprintf("Expected hf:<org>/<repo>[:<file-or-quant>], got %q.", value)}
	}
	if !ValidRepoID(m[1]) {
		return Reference{}, &Error{Message: fmt.Sprintf("%q is not a Hugging Face repository name (expected <org>/<repo>).", m[1])}
	}
	return Reference{Repo: m[1], Selector: m[2]}, nil
}

// File is a repository file stored in LFS (size and sha256 known).
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

var (
	shardRe = regexp.MustCompile(`(?i)-(\d{5})-of-(\d{5})\.gguf$`)
	// quantRe is TS QUANT with its trailing lookahead (?=[-_.]|$) consumed.
	quantRe         = regexp.MustCompile(`(?i)(?:^|[-_.])((?:IQ|Q)\d(?:_[A-Z0-9]+)*|BF16|F16|F32|MXFP4(?:_MOE)?)(?:[-_.]|$)`)
	weightExtRe     = regexp.MustCompile(`(?i)\.(gguf|safetensors|ckpt|bin)$`)
	auxiliaryRe     = regexp.MustCompile(`(?i)(?:^|[/_.-])(?:mmproj|projectors?|mtp|draft|imatrix|importance[-_]matrix|adapters?|lora|tokenizers?|vocab)(?:[/_.-]|$)`)
	preferredQuants = []string{"Q4_K_M", "Q4_K_S", "Q5_K_M", "Q8_0"}
	imageQuants     = []string{"Q8_0", "Q5_0", "Q5_1", "Q4_K", "Q4_0"}
	pipelineQuants  = []string{"Q4_0", "Q4_K", "Q4_K_S", "Q5_0", "Q8_0"}
)

// lfsFiles lists the repository's LFS files.
func lfsFiles(info *ModelInfo) []File {
	var out []File
	for _, s := range info.Siblings {
		if s.LFS != nil {
			out = append(out, File{s.RFilename, s.LFS.Size, strings.ToLower(s.LFS.SHA256)})
		}
	}
	return out
}

// QuantOf is the quantization a weights file name declares ("" if none).
func QuantOf(p string) string {
	m := quantRe.FindStringSubmatch(weightExtRe.ReplaceAllString(path.Base(p), ""))
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1])
}

func auxiliaryGGUF(p string) bool {
	return strings.Contains(strings.ToLower(path.Base(p)), "mmproj") || auxiliaryRe.MatchString(p)
}

var (
	slugInvalidRe = regexp.MustCompile(`[^a-z0-9._-]+`)
	slugTrimRe    = regexp.MustCompile(`^[-._]+|[-._]+$`)
	ggmlPrefixRe  = regexp.MustCompile(`(?i)^ggml[-_]?`)
	// sizeRe is TS sizeOf with its trailing lookahead consumed.
	sizeRe      = regexp.MustCompile(`(?i)(?:^|[-_.])(\d+(?:\.\d+)?)([BM])(?:[-_.]|$)`)
	whisperSize = regexp.MustCompile(`(?i)(tiny|base|small|medium|large(?:-v\d)?(?:-turbo)?)`)
	modelIDRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	ggufSuffix  = regexp.MustCompile(`(?i)[-_]?GGUF$`)
	coderRe     = regexp.MustCompile(`(?i)coder|code`)
	englishRe   = regexp.MustCompile(`(?i)(^|[-_.])en($|[-_.])`)
)

func slug(v string) string {
	return slugTrimRe.ReplaceAllString(slugInvalidRe.ReplaceAllString(strings.ToLower(v), "-"), "")
}

// ModelIDFor is the catalog id derived from a weights file name.
func ModelIDFor(p, kind string) string {
	stem := weightExtRe.ReplaceAllString(shardRe.ReplaceAllString(path.Base(p), ""), "")
	if kind == "stt" {
		variant := slug(ggmlPrefixRe.ReplaceAllString(stem, ""))
		if strings.Contains(variant, "whisper") {
			return variant
		}
		return "whisper-" + variant
	}
	return slug(stem)
}
