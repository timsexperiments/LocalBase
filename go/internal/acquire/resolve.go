package acquire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/models"
)

// Warning codes shown before a download.
const (
	WarnCustomSource = "custom-source"
	WarnInsecureHTTP = "insecure-http"
	WarnLocalFile    = "local-file"
	WarnUnknownSize  = "unknown-size"
)

// CustomSourceWarning is the spec's text for unverified sources.
const CustomSourceWarning = "This file comes from a custom source that LocalBase has not verified. Only download files you trust. " +
	"A successful download does not guarantee the model is safe or compatible."

// InsecureHTTPWarning is shown for plain HTTP.
const InsecureHTTPWarning = "This address uses plain HTTP. The file and any headers you send can be read or changed in transit."

// Warning is shown to the person importing.
type Warning struct {
	Code string `json:"code"`
	Text string `json:"text"`
}

// Request is an import request.
type Request struct {
	Source      string   // hf:org/repo, https://huggingface.co/org/repo[/blob/rev/file], https://..., http://..., or a local path
	Files       []string // HF: files in the repo to fetch (weights; companions are added)
	Revision    string   // HF: branch, tag or commit; resolved to a commit
	SHA256      string   // a trusted checksum for a single-file source
	AuthProfile string
	AllowHTTP   bool
	ID          string // model id; derived when empty
	Name        string
	Task        string // when the source does not say
	HFToken     string

	hfOrigins []string // extra origins for the token (the "huggingface" profile)
}

// PlannedFile is one file to fetch.
type PlannedFile struct {
	Filename  string        `json:"filename"`
	Role      string        `json:"role"`
	Required  bool          `json:"required"`
	SizeBytes int64         `json:"sizeBytes"` // 0 = unknown
	SHA256    string        `json:"sha256,omitempty"`
	HashTrust string        `json:"hashTrust,omitempty"`
	Source    models.Source `json:"source"`
	fetchURL  string        // with credentials-free query intact; never shown
	local     string        // local source path
}

// Plan is a resolved source, shown before downloading.
type Plan struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Kind       string        `json:"kind"` // huggingface | url | local
	Origin     string        `json:"origin"`
	Display    string        `json:"display"` // redacted source for display
	Revision   string        `json:"revision,omitempty"`
	Custom     bool          `json:"custom"`
	Task       string        `json:"task,omitempty"`
	License    string        `json:"license,omitempty"`
	Warnings   []Warning     `json:"warnings"`
	Files      []PlannedFile `json:"files"`
	TotalBytes int64         `json:"totalBytes"`
	SizeKnown  bool          `json:"sizeKnown"`
	// Choices lists the weight files to pick from when the repository has
	// several and none was named (no files are planned then).
	Choices []string `json:"choices,omitempty"`
	hfOrigins []string
	// Preview holds the facts known before downloading (pinned upstream
	// metadata, or a local file's header), for a backend recommendation in
	// the import form. Downloaded files are inspected again.
	Preview models.Variant `json:"preview"`
	profile *Profile
	hfToken string
}

var (
	hfRepoRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	shardRe       = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)
	sha256Re      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	filenameClean = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// Never fetched: repository code and pickled weights, which execute code
// when loaded by some tools.
var refusedExt = []string{".py", ".pyc", ".pkl", ".pickle", ".pt", ".pth", ".ckpt", ".sh", ".exe", ".so", ".dylib", ".dll", ".ipynb"}

func refused(name string) bool {
	return slices.Contains(refusedExt, strings.ToLower(path.Ext(name)))
}

// Resolve turns a request into a plan without downloading weights.
func Resolve(ctx context.Context, rootDir string, req Request) (*Plan, error) {
	var profile *Profile
	if req.AuthProfile != "" {
		ps, err := LoadProfiles(rootDir)
		if err != nil {
			return nil, err
		}
		if profile = ps.Get(req.AuthProfile); profile == nil {
			return nil, fmt.Errorf("auth profile %q does not exist", req.AuthProfile)
		}
	}
	if req.SHA256 != "" {
		req.SHA256 = strings.ToLower(strings.TrimSpace(req.SHA256))
		if !sha256Re.MatchString(req.SHA256) {
			return nil, errors.New("--sha256 must be 64 hexadecimal characters")
		}
	}
	req.hfOrigins = hfTokenOrigins(rootDir)
	src := strings.TrimSpace(req.Source)
	var p *Plan
	var err error
	switch {
	case strings.HasPrefix(src, "hf:"):
		p, err = resolveHF(ctx, strings.TrimPrefix(src, "hf:"), "", req, profile)
	case strings.HasPrefix(src, HFEndpoint()+"/") || strings.HasPrefix(src, "https://huggingface.co/"):
		repo, file, rev := parseHFURL(src)
		if rev != "" && req.Revision == "" {
			req.Revision = rev
		}
		p, err = resolveHF(ctx, repo, file, req, profile)
	case strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "http://"):
		p, err = resolveURL(ctx, src, req, profile)
	case hfRepoRe.MatchString(src) && !fileExists(src):
		p, err = resolveHF(ctx, src, "", req, profile)
	default:
		p, err = resolveLocal(src, req)
	}
	if err != nil {
		return nil, err
	}
	p.profile, p.hfToken, p.hfOrigins = profile, req.HFToken, req.hfOrigins
	if req.Name != "" {
		p.Name = req.Name
	}
	if req.ID != "" {
		p.ID = req.ID
	}
	if !models.ValidID(p.ID) {
		return nil, fmt.Errorf("model id %q must be lowercase letters, digits, '.', '_' or '-' (set one with --id)", p.ID)
	}
	if req.Task != "" {
		p.Task = req.Task
	}
	p.SizeKnown = true
	for _, f := range p.Files {
		if f.SizeBytes <= 0 {
			p.SizeKnown = false
		}
		p.TotalBytes += f.SizeBytes
	}
	p.Preview.ID = "preview"
	for _, f := range p.Files {
		p.Preview.Artifacts = append(p.Preview.Artifacts, models.Artifact{Filename: f.Filename, Role: f.Role, Required: f.Required, SizeBytes: f.SizeBytes})
	}
	if !p.SizeKnown && len(p.Files) > 0 {
		p.Warnings = append(p.Warnings, Warning{WarnUnknownSize, "The source does not report the size of every file; it is checked after download."})
	}
	return p, nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func parseHFURL(raw string) (repo, file, rev string) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", ""
	}
	repo = parts[0] + "/" + parts[1]
	if len(parts) >= 5 && (parts[2] == "blob" || parts[2] == "resolve") {
		rev, file = parts[3], strings.Join(parts[4:], "/")
	}
	return repo, file, rev
}

func deriveID(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimSuffix(s, ".gguf")
	s = filenameClean.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-._")
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}

var pipelineTasks = map[string]string{
	"text-generation": "chat", "image-text-to-text": "chat", "feature-extraction": "embeddings", "sentence-similarity": "embeddings",
	"automatic-speech-recognition": "transcription", "text-to-speech": "speech", "text-to-image": "image", "image-to-image": "image",
	"text-to-video": "video", "image-to-video": "video",
}

type hfInfo struct {
	SHA         string `json:"sha"`
	PipelineTag string `json:"pipeline_tag"`
	GGUF        *struct {
		Architecture  string `json:"architecture"`
		ContextLength int    `json:"context_length"`
	} `json:"gguf"`
	CardData struct {
		License any `json:"license"`
	} `json:"cardData"`
	Siblings []struct {
		RFilename string `json:"rfilename"`
		Size      int64  `json:"size"`
		LFS       *struct {
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
		} `json:"lfs"`
	} `json:"siblings"`
}

func resolveHF(ctx context.Context, repo, file string, req Request, profile *Profile) (*Plan, error) {
	if !hfRepoRe.MatchString(repo) {
		return nil, fmt.Errorf("%q is not a Hugging Face repository (owner/name)", repo)
	}
	rev := req.Revision
	if rev == "" {
		rev = "main"
	}
	client := newClient(profile, req.HFToken, req.hfOrigins)
	api := HFEndpoint() + "/api/models/" + repo + "/revision/" + url.PathEscape(rev) + "?blobs=true"
	hreq, _ := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	hreq.Header.Set("Accept", "application/json")
	resp, err := client.Do(hreq)
	if err != nil {
		return nil, redactErr(err, nil)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
	case 401, 403:
		return nil, fmt.Errorf("Hugging Face refused access to %s (HTTP %d); it may be gated or private: accept its terms and save a token", repo, resp.StatusCode)
	case 404:
		return nil, fmt.Errorf("Hugging Face has no repository %s at revision %s", repo, rev)
	default:
		return nil, fmt.Errorf("Hugging Face answered HTTP %d for %s", resp.StatusCode, repo)
	}
	var info hfInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&info); err != nil || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(info.SHA) {
		return nil, errors.New("Hugging Face returned no immutable commit for that revision")
	}
	origin, _ := Origin(HFEndpoint())
	p := &Plan{Kind: "huggingface", Origin: origin, Revision: info.SHA, Display: "hf:" + repo + "@" + info.SHA,
		Task: pipelineTasks[info.PipelineTag], Warnings: []Warning{}}
	if l, ok := info.CardData.License.(string); ok {
		p.License = l
	}
	if g := info.GGUF; g != nil {
		p.Preview.Format = models.Of("gguf", models.SourcePinned)
		if g.Architecture != "" {
			p.Preview.Architecture = models.Of(g.Architecture, models.SourcePinned)
		}
		if g.ContextLength > 0 {
			p.Preview.ContextLength = models.Of(g.ContextLength, models.SourcePinned)
		}
	}
	type sib struct {
		name   string
		size   int64
		sha256 string
	}
	var sibs []sib
	byName := map[string]sib{}
	for _, s := range info.Siblings {
		x := sib{name: s.RFilename, size: s.Size}
		if s.LFS != nil {
			x.sha256 = strings.ToLower(s.LFS.SHA256)
			if s.LFS.Size > 0 {
				x.size = s.LFS.Size
			}
		}
		sibs = append(sibs, x)
		byName[x.name] = x
	}
	files := req.Files
	if file != "" {
		files = append([]string{file}, files...)
	}
	isGGUF := func(n string) bool { return strings.HasSuffix(strings.ToLower(n), ".gguf") }
	isProjector := func(n string) bool { return isGGUF(n) && strings.Contains(strings.ToLower(path.Base(n)), "mmproj") }
	if len(files) == 0 {
		// Weight sets in the repository: single GGUFs and split sets.
		var sets []string
		for _, s := range sibs {
			if !isGGUF(s.name) || isProjector(s.name) {
				continue
			}
			if m := shardRe.FindStringSubmatch(path.Base(s.name)); m != nil && m[2] != "00001" {
				continue
			}
			sets = append(sets, s.name)
		}
		switch {
		case len(sets) == 1:
			files = sets
		case len(sets) > 1:
			sort.Strings(sets)
			p.Choices = sets
			p.ID, p.Name = deriveID(path.Base(repo)), path.Base(repo)
			return p, nil
		default:
			// A non-GGUF repository: its data files (weights, configs,
			// tokenizers), never code or pickles.
			for _, s := range sibs {
				if !refused(s.name) && !strings.HasPrefix(path.Base(s.name), ".") && !strings.EqualFold(path.Base(s.name), "README.md") {
					files = append(files, s.name)
				}
			}
			if len(files) == 0 {
				return nil, fmt.Errorf("%s has no model files LocalBase can download", repo)
			}
		}
	}
	add := func(name, role string, required bool) error {
		if refused(name) {
			return fmt.Errorf("%s is code or a pickle, which LocalBase never downloads", name)
		}
		s, ok := byName[name]
		if !ok {
			return fmt.Errorf("%s has no file %s at %s", repo, name, info.SHA)
		}
		for _, f := range p.Files {
			if f.Source.Path == name {
				return nil
			}
		}
		base := path.Base(name)
		for _, f := range p.Files {
			if f.Filename == base {
				return fmt.Errorf("two files are both named %s; import them separately", base)
			}
		}
		pf := PlannedFile{Filename: base, Role: role, Required: required, SizeBytes: s.size,
			Source:   models.Source{Kind: "huggingface", Repo: repo, Revision: info.SHA, Path: name, AuthProfile: req.AuthProfile},
			fetchURL: HFEndpoint() + "/" + repo + "/resolve/" + info.SHA + "/" + escapePath(name)}
		if s.sha256 != "" {
			pf.SHA256, pf.HashTrust = s.sha256, models.HashAuthoritative
		}
		p.Files = append(p.Files, pf)
		return nil
	}
	for _, f := range files {
		role := roleFor(f)
		if err := add(f, role, true); err != nil {
			return nil, err
		}
		// The other shards of a split set.
		if m := shardRe.FindStringSubmatch(path.Base(f)); m != nil {
			n, _ := strconv.Atoi(m[3])
			dir := path.Dir(f)
			for i := 1; i <= n; i++ {
				name := path.Join(dir, fmt.Sprintf("%s-%05d-of-%05d.gguf", m[1], i, n))
				if strings.HasPrefix(name, "./") {
					name = name[2:]
				}
				if err := add(name, models.RoleWeights, true); err != nil {
					return nil, fmt.Errorf("incomplete split set: %v", err)
				}
			}
		}
	}
	// A single projector next to GGUF weights is the vision/audio companion.
	if len(files) > 0 && isGGUF(files[0]) && !isProjector(files[0]) {
		var projectors []string
		for _, s := range sibs {
			if isProjector(s.name) {
				projectors = append(projectors, s.name)
			}
		}
		if len(projectors) == 1 {
			if err := add(projectors[0], models.RoleProjector, false); err != nil {
				return nil, err
			}
		}
	}
	if req.SHA256 != "" {
		if len(p.Files) != 1 {
			return nil, errors.New("--sha256 applies to a single file; this import has several")
		}
		if p.Files[0].SHA256 != "" && p.Files[0].SHA256 != req.SHA256 {
			return nil, errors.New("the checksum you gave differs from the one Hugging Face publishes for this file")
		}
		p.Files[0].SHA256, p.Files[0].HashTrust = req.SHA256, models.HashUser
	}
	p.ID = deriveID(path.Base(files[0]))
	p.Name = strings.TrimSuffix(path.Base(files[0]), path.Ext(files[0]))
	return p, nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func roleFor(name string) string {
	base := strings.ToLower(path.Base(name))
	switch {
	case strings.Contains(base, "mmproj"):
		return models.RoleProjector
	case strings.HasPrefix(base, "tokenizer") || base == "vocab.json" || base == "merges.txt" || base == "spiece.model":
		return models.RoleTokenizer
	case strings.HasSuffix(base, ".json"):
		return models.RoleConfig
	case strings.Contains(base, "vae"):
		return models.RoleVAE
	}
	return models.RoleWeights
}

func resolveURL(ctx context.Context, raw string, req Request, profile *Profile) (*Plan, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, errors.New("that is not a valid URL")
	}
	if u.User != nil {
		return nil, errors.New("put credentials in an auth profile, not in the URL")
	}
	origin, _ := Origin(raw)
	p := &Plan{Kind: "url", Origin: origin, Custom: true, Display: RedactURL(raw),
		Warnings: []Warning{{WarnCustomSource, CustomSourceWarning}}}
	if u.Scheme == "http" {
		if !req.AllowHTTP {
			return nil, errors.New("this is a plain HTTP address; confirm the insecure transfer (--allow-http) to continue")
		}
		p.Warnings = append(p.Warnings, Warning{WarnInsecureHTTP, InsecureHTTPWarning})
	}
	if profile != nil && !profile.Allows(origin) {
		return nil, fmt.Errorf("auth profile %s is not allowed for %s; add the origin to the profile", profile.ID, origin)
	}
	client := newClient(profile, "", nil)
	size, name := int64(0), path.Base(u.Path)
	hreq, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	hreq.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(hreq)
	if err != nil {
		return nil, redactErr(err, redactorFor(profile))
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	switch {
	case resp.StatusCode == 206:
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if i := strings.LastIndex(cr, "/"); i >= 0 {
				size, _ = strconv.ParseInt(cr[i+1:], 10, 64)
			}
		}
	case resp.StatusCode == 200:
		if resp.Header.Get("Content-Encoding") == "" {
			size = resp.ContentLength
		}
	default:
		return nil, fmt.Errorf("%s answered HTTP %d", origin, resp.StatusCode)
	}
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		name = path.Base(params["filename"])
	}
	name = filenameClean.ReplaceAllString(name, "_")
	if name == "" || name == "." || name == "/" || name == "_" {
		name = "model.bin"
	}
	if refused(name) {
		return nil, fmt.Errorf("%s is code or a pickle, which LocalBase never downloads", name)
	}
	if size < 0 {
		size = 0
	}
	pf := PlannedFile{Filename: name, Role: models.RoleWeights, Required: true, SizeBytes: size,
		Source: models.Source{Kind: "url", URL: RedactURL(raw), AuthProfile: req.AuthProfile}, fetchURL: raw}
	if req.SHA256 != "" {
		pf.SHA256, pf.HashTrust = req.SHA256, models.HashUser
	}
	p.Files = []PlannedFile{pf}
	p.ID, p.Name = deriveID(name), strings.TrimSuffix(name, path.Ext(name))
	return p, nil
}

func resolveLocal(src string, req Request) (*Plan, error) {
	abs, err := filepath.Abs(src)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a file, a URL or a Hugging Face repository", src)
	}
	name := filepath.Base(abs)
	if refused(name) {
		return nil, fmt.Errorf("%s is code or a pickle, which LocalBase never imports", name)
	}
	p := &Plan{Kind: "local", Origin: "file://", Display: abs, Warnings: []Warning{{WarnLocalFile,
		"LocalBase copies this file; it cannot tell where it came from. Import files you trust."}}}
	pf := PlannedFile{Filename: filenameClean.ReplaceAllString(name, "_"), Role: models.RoleWeights, Required: true, SizeBytes: info.Size(),
		Source: models.Source{Kind: "local", Path: abs}, local: abs}
	if req.SHA256 != "" {
		pf.SHA256, pf.HashTrust = req.SHA256, models.HashUser
	}
	// Other shards of a split GGUF next to it come along.
	if m := shardRe.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[3])
		pf.Filename = name
		p.Files = append(p.Files, pf)
		for i := 1; i <= n; i++ {
			other := fmt.Sprintf("%s-%05d-of-%05d.gguf", m[1], i, n)
			if other == name {
				continue
			}
			op := filepath.Join(filepath.Dir(abs), other)
			oi, err := os.Stat(op)
			if err != nil {
				return nil, fmt.Errorf("incomplete split set: %s is missing next to %s", other, name)
			}
			p.Files = append(p.Files, PlannedFile{Filename: other, Role: models.RoleWeights, Required: true, SizeBytes: oi.Size(),
				Source: models.Source{Kind: "local", Path: op}, local: op})
		}
		sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Filename < p.Files[j].Filename })
	} else {
		p.Files = []PlannedFile{pf}
	}
	first := p.Files[0]
	ff := models.InspectFile(first.local)
	p.Preview = models.Variant{Format: ff.Format, Architecture: ff.Architecture, Precision: ff.Precision, ContextLength: ff.ContextLength}
	p.ID, p.Name = deriveID(name), strings.TrimSuffix(name, filepath.Ext(name))
	return p, nil
}

func redactorFor(p *Profile) *Redactor {
	r := &Redactor{}
	if p != nil {
		for _, v := range p.Headers {
			r.Add(v)
		}
	}
	return r
}
