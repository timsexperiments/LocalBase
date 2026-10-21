package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	cacheTTL       = 10 * time.Minute
	requestTimeout = 15 * time.Second
)

// Client talks to the Hub for one root: responses are cached under
// <root>/cache/hf/ for ten minutes, and a failed request falls back to the
// cached copy when one exists.
type Client struct {
	Root string
	// Token is sent only to the Hub endpoint, for gated repositories.
	Token string
	// Endpoint is the Hub API origin (default HF_ENDPOINT, else the public
	// Hub); tests point it at a local server.
	Endpoint string
	HTTP     *http.Client
	Now      func() time.Time
}

func (c *Client) endpoint() string {
	if c.Endpoint != "" {
		return strings.TrimSuffix(c.Endpoint, "/")
	}
	if e := os.Getenv("HF_ENDPOINT"); e != "" {
		return strings.TrimSuffix(e, "/")
	}
	return PublicEndpoint
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Error is a Hub failure explained in plain words. Code is
// hub_auth_required or hub_rate_limited when a token might help.
type Error struct{ Message, Code string }

func (e *Error) Error() string { return e.Message }

type cacheEntry struct {
	URL       string          `json:"url"`
	FetchedAt float64         `json:"fetchedAt"`
	Body      json.RawMessage `json:"body"`
}

func (c *Client) cachePath(u string) string {
	sum := sha256.Sum256([]byte(u))
	return filepath.Join(c.Root, "cache", "hf", hex.EncodeToString(sum[:])[:32]+".json")
}

func (c *Client) readCache(u string) *cacheEntry {
	b, err := os.ReadFile(c.cachePath(u))
	if err != nil {
		return nil
	}
	var e cacheEntry
	if json.Unmarshal(b, &e) != nil || e.URL != u || e.Body == nil {
		return nil
	}
	return &e
}

func (c *Client) writeCache(u string, body []byte, now time.Time) {
	// Never create files in a directory that is not a LocalBase root yet.
	if _, err := os.Stat(filepath.Join(c.Root, ".localbase-root.json")); err != nil {
		return
	}
	dir := filepath.Join(c.Root, "cache", "hf")
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	b, _ := json.Marshal(cacheEntry{URL: u, FetchedAt: float64(now.UnixMilli()), Body: body})
	path := c.cachePath(u)
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, path)
	}
}

// getJSON fetches JSON with caching and offline-safe errors.
func (c *Client) getJSON(ctx context.Context, u, what string) (json.RawMessage, bool, error) {
	now := c.now()
	cached := c.readCache(u)
	if cached != nil && float64(now.UnixMilli())-cached.FetchedAt < float64(cacheTTL.Milliseconds()) {
		return cached.Body, true, nil
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("Accept", "application/json")
	if pu, err := url.Parse(u); err == nil && c.Token != "" && pu.Scheme+"://"+pu.Host == c.endpoint() {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if cached != nil {
			return cached.Body, true, nil
		}
		msg := err.Error()
		var ue *url.Error
		if errors.As(err, &ue) {
			msg = ue.Err.Error()
		}
		return nil, false, &Error{Message: fmt.Sprintf("Could not reach Hugging Face to %s (%s). Check the network connection and try again; nothing was changed.", what, msg)}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if resp.StatusCode >= 500 && cached != nil {
			return cached.Body, true, nil
		}
		status := resp.StatusCode
		auth := status == 401 || status == 403
		e := &Error{}
		switch {
		case status == 404:
			e.Message = fmt.Sprintf("Hugging Face has no public repository for that name (%s).", what)
		case auth:
			e.Message = fmt.Sprintf("Hugging Face refused access (%d) while trying to %s. The repository may be gated: accept its terms on huggingface.co and save a token with `local-base configure --hf-token`.", status, what)
			e.Code = "hub_auth_required"
		case status == 429:
			e.Message = fmt.Sprintf("Hugging Face answered 429 (rate limited) while trying to %s. Wait a minute, or save a Hugging Face token for a higher limit; nothing was changed.", what)
			e.Code = "hub_rate_limited"
		default:
			e.Message = fmt.Sprintf("Hugging Face answered %d while trying to %s. Try again later; nothing was changed.", status, what)
		}
		return nil, false, e
	}
	if !json.Valid(body) {
		return nil, false, &Error{Message: fmt.Sprintf("Hugging Face sent a response that is not JSON while trying to %s.", what)}
	}
	c.writeCache(u, body, now)
	return body, false, nil
}

// --- model information ----------------------------------------------------

var (
	repoIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	shaRe    = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
	sha256Re = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
)

// ValidRepoID reports an <org>/<repo> name.
func ValidRepoID(id string) bool {
	if !repoIDRe.MatchString(id) {
		return false
	}
	for _, p := range strings.Split(id, "/") {
		if p == "." || p == ".." {
			return false
		}
	}
	return true
}

// ErrInvalidRepo: the name is not <org>/<repo>.
var ErrInvalidRepo = errors.New("expected <org>/<repo>")

// Sibling is one file of a repository.
type Sibling struct {
	RFilename string `json:"rfilename"`
	LFS       *struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	} `json:"lfs,omitempty"`
}

// ModelInfo is the Hub's model information (the fields LocalBase reads).
type ModelInfo struct {
	ID          string
	SHA         string
	PipelineTag string
	Tags        []string
	Library     string
	Gated       bool
	CardLicense any
	CardData    map[string]any
	GGUF        struct {
		Architecture  string
		ContextLength int64
		ChatTemplate  string
	}
	Siblings []Sibling
	// SchedulerPrediction is set by LocalBase: eps or v, from the diffusers
	// scheduler config.
	SchedulerPrediction string
}

func isInt(v any, min float64) (int64, bool) {
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || f < min || math.Abs(f) > 1<<53-1 {
		return 0, false
	}
	return int64(f), true
}

// IsGated: the Hub reports false, "auto" or "manual".
func IsGated(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != "false"
	}
	return true
}

// parseModelInfo validates like the TS modelInfoSchema; false when the
// response is not one LocalBase understands.
func parseModelInfo(body []byte) (*ModelInfo, bool) {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil || raw == nil {
		return nil, false
	}
	str := func(k string) (string, bool) {
		v, present := raw[k]
		if !present {
			return "", true
		}
		s, ok := v.(string)
		return s, ok
	}
	info := &ModelInfo{}
	var ok bool
	if info.ID, ok = raw["id"].(string); !ok || info.ID == "" {
		return nil, false
	}
	if info.SHA, ok = raw["sha"].(string); !ok || !shaRe.MatchString(info.SHA) {
		return nil, false
	}
	if info.PipelineTag, ok = str("pipeline_tag"); !ok {
		return nil, false
	}
	if info.Library, ok = str("library_name"); !ok {
		return nil, false
	}
	if t, present := raw["tags"]; present {
		arr, ok := t.([]any)
		if !ok {
			return nil, false
		}
		for _, x := range arr {
			s, ok := x.(string)
			if !ok {
				return nil, false
			}
			info.Tags = append(info.Tags, s)
		}
	}
	if info.Tags == nil {
		info.Tags = []string{}
	}
	if g, present := raw["gated"]; present {
		switch g.(type) {
		case bool, string:
		default:
			return nil, false
		}
		info.Gated = IsGated(g)
	}
	if cd, present := raw["cardData"]; present {
		m, ok := cd.(map[string]any)
		if !ok {
			return nil, false
		}
		info.CardData = m
		if l, present := m["license"]; present {
			switch x := l.(type) {
			case string:
			case []any:
				for _, e := range x {
					if _, ok := e.(string); !ok {
						return nil, false
					}
				}
			default:
				return nil, false
			}
			info.CardLicense = l
		}
	}
	if g, present := raw["gguf"]; present {
		m, ok := g.(map[string]any)
		if !ok {
			return nil, false
		}
		if a, present := m["architecture"]; present {
			if info.GGUF.Architecture, ok = a.(string); !ok {
				return nil, false
			}
		}
		if cl, present := m["context_length"]; present {
			if info.GGUF.ContextLength, ok = isInt(cl, 1); !ok {
				return nil, false
			}
		}
		if ct, present := m["chat_template"]; present {
			if info.GGUF.ChatTemplate, ok = ct.(string); !ok {
				return nil, false
			}
		}
	}
	if s, present := raw["siblings"]; present {
		arr, ok := s.([]any)
		if !ok {
			return nil, false
		}
		for _, x := range arr {
			m, ok := x.(map[string]any)
			if !ok {
				return nil, false
			}
			var sib Sibling
			if sib.RFilename, ok = m["rfilename"].(string); !ok || sib.RFilename == "" {
				return nil, false
			}
			if sz, present := m["size"]; present {
				if _, ok := isInt(sz, 0); !ok {
					return nil, false
				}
			}
			if l, present := m["lfs"]; present {
				lm, ok := l.(map[string]any)
				if !ok {
					return nil, false
				}
				sha, okS := lm["sha256"].(string)
				size, okZ := isInt(lm["size"], 1)
				if !okS || !sha256Re.MatchString(sha) || !okZ {
					return nil, false
				}
				sib.LFS = &struct {
					SHA256 string `json:"sha256"`
					Size   int64  `json:"size"`
				}{sha, size}
			}
			info.Siblings = append(info.Siblings, sib)
		}
	}
	return info, true
}

const schedulerConfig = "scheduler/scheduler_config.json"

// ModelInfo fetches a repository's information (with the scheduler
// prediction of single-file image repositories).
func (c *Client) ModelInfo(ctx context.Context, repo string) (*ModelInfo, error) {
	if !ValidRepoID(repo) {
		return nil, ErrInvalidRepo
	}
	body, _, err := c.getJSON(ctx, c.endpoint()+"/api/models/"+repo+"?blobs=true", "look up "+repo)
	if err != nil {
		return nil, err
	}
	info, ok := parseModelInfo(body)
	if !ok {
		return nil, &Error{Message: fmt.Sprintf("Hugging Face returned model information for %s that LocalBase does not understand.", repo)}
	}
	info.SchedulerPrediction = c.schedulerPrediction(ctx, info)
	return info, nil
}

func (c *Client) schedulerPrediction(ctx context.Context, info *ModelInfo) string {
	found := false
	for _, s := range info.Siblings {
		if s.RFilename == schedulerConfig {
			found = true
		}
	}
	if !found {
		return ""
	}
	likely := LikelyKind(info.PipelineTag, info.Tags, info.ID, info.Library)
	if likely.Kind == nil || *likely.Kind != "image" || likely.Pipeline != "" {
		return ""
	}
	body, _, err := c.getJSON(ctx, c.endpoint()+"/"+info.ID+"/resolve/"+info.SHA+"/"+schedulerConfig, "read the scheduler config of "+info.ID)
	if err != nil {
		return ""
	}
	var cfg struct {
		PredictionType any `json:"prediction_type"`
	}
	json.Unmarshal(body, &cfg)
	switch cfg.PredictionType {
	case "v_prediction":
		return "v"
	case "epsilon":
		return "eps"
	}
	return ""
}

// --- search -------------------------------------------------------------------

// Hit is one search result.
type Hit struct {
	Repo        string  `json:"repo"`
	Downloads   int64   `json:"downloads"`
	Likes       *int64  `json:"likes,omitempty"`
	License     *string `json:"license"`
	PipelineTag *string `json:"pipelineTag"`
	LibraryName *string `json:"libraryName"`
	Gated       bool    `json:"gated"`
	SHA         string  `json:"sha,omitempty"`
	Likely      Likely  `json:"likely"`
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

var searchFields = []string{"downloads", "likes", "pipeline_tag", "tags", "gated", "library_name", "sha"}

// urlSearchParams encodes like URLSearchParams (spaces as +).
func urlSearchParams(pairs [][2]string) string {
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(formEncode(p[0]) + "=" + formEncode(p[1]))
	}
	return b.String()
}

func formEncode(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '*', c == '-', c == '.', c == '_':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func (c *Client) searchPage(ctx context.Context, limit int, search, filter, pipelineTag string, optional bool) ([]Hit, bool, error) {
	pairs := [][2]string{{"sort", "downloads"}, {"direction", "-1"}, {"limit", strconv.Itoa(limit)}}
	if filter != "" {
		pairs = append(pairs, [2]string{"filter", filter})
	}
	if pipelineTag != "" {
		pairs = append(pairs, [2]string{"pipeline_tag", pipelineTag})
	}
	if search != "" {
		pairs = append(pairs, [2]string{"search", search})
	}
	for _, f := range searchFields {
		pairs = append(pairs, [2]string{"expand[]", f})
	}
	body, cached, err := c.getJSON(ctx, c.endpoint()+"/api/models?"+urlSearchParams(pairs), "search models")
	if err != nil {
		if optional {
			return nil, true, nil
		}
		return nil, false, err
	}
	hits, ok := parseHits(body)
	if !ok {
		if optional {
			return nil, true, nil
		}
		return nil, false, &Error{Message: "Hugging Face returned search results LocalBase does not understand."}
	}
	return hits, cached, nil
}

func parseHits(body []byte) ([]Hit, bool) {
	var raw []map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return nil, false
	}
	hits := []Hit{}
	for _, m := range raw {
		if m == nil {
			return nil, false
		}
		id, ok := m["id"].(string)
		if !ok || id == "" {
			return nil, false
		}
		h := Hit{Repo: id}
		if d, present := m["downloads"]; present {
			if h.Downloads, ok = isInt(d, 0); !ok {
				return nil, false
			}
		}
		if l, present := m["likes"]; present {
			n, ok := isInt(l, 0)
			if !ok {
				return nil, false
			}
			h.Likes = &n
		}
		strField := func(k string) (string, bool) {
			v, present := m[k]
			if !present {
				return "", true
			}
			s, ok := v.(string)
			return s, ok
		}
		pipeline, ok1 := strField("pipeline_tag")
		library, ok2 := strField("library_name")
		sha, ok3 := strField("sha")
		if !ok1 || !ok2 || !ok3 {
			return nil, false
		}
		var tags []string
		if t, present := m["tags"]; present {
			arr, ok := t.([]any)
			if !ok {
				return nil, false
			}
			for _, x := range arr {
				s, ok := x.(string)
				if !ok {
					return nil, false
				}
				tags = append(tags, s)
			}
		}
		if g, present := m["gated"]; present {
			switch g.(type) {
			case bool, string:
			default:
				return nil, false
			}
			h.Gated = IsGated(g)
		}
		h.License = strPtr(License(tags, nil))
		h.PipelineTag, h.LibraryName = strPtr(pipeline), strPtr(library)
		if shaRe.MatchString(sha) {
			h.SHA = strings.ToLower(sha)
		}
		h.Likely = LikelyKind(pipeline, tags, id, library)
		hits = append(hits, h)
	}
	return hits, true
}

// Search searches the Hub, most downloaded first: GGUF repositories for
// language models, recognition repositories for speech, text-to-image ones
// for images (plus image-to-image and GGUF repositories of a pipeline
// LocalBase can launch).
func (c *Client) Search(ctx context.Context, kind, query string, limit int) ([]Hit, bool, error) {
	if kind == "" {
		kind = "llm"
	}
	if limit <= 0 {
		limit = 20
	}
	var words []string
	for _, w := range strings.Fields(query) {
		if kind == "llm" && strings.ToLower(w) == "gguf" {
			continue
		}
		words = append(words, w)
	}
	search := strings.Join(words, " ")
	if search == "" && kind == "stt" {
		search = "whisper"
	}
	type q struct{ filter, pipeline string }
	var queries []q
	switch kind {
	case "llm":
		queries = []q{{filter: "gguf"}}
	case "stt":
		queries = []q{{pipeline: "automatic-speech-recognition"}}
	default:
		queries = []q{{pipeline: "text-to-image"}, {pipeline: "image-to-image"}}
		if search != "" {
			queries = append(queries, q{filter: "gguf"})
		}
	}
	type page struct {
		hits   []Hit
		cached bool
		err    error
	}
	pages := make([]page, len(queries))
	done := make(chan int, len(queries))
	for i, qq := range queries {
		go func(i int, qq q) {
			h, cached, err := c.searchPage(ctx, limit, search, qq.filter, qq.pipeline, i > 0)
			pages[i] = page{h, cached, err}
			done <- i
		}(i, qq)
	}
	for range queries {
		<-done
	}
	if pages[0].err != nil {
		return nil, false, pages[0].err
	}
	seen := map[string]bool{}
	var hits []Hit
	allCached := true
	for i, p := range pages {
		allCached = allCached && p.cached
		for _, h := range p.hits {
			// Extra image searches only add repositories of a launchable pipeline.
			if i > 0 && h.Likely.Pipeline == "" {
				continue
			}
			if !seen[h.Repo] {
				seen[h.Repo] = true
				hits = append(hits, h)
			}
		}
	}
	if len(pages) > 1 {
		sort.SliceStable(hits, func(a, b int) bool { return hits[a].Downloads > hits[b].Downloads })
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	if hits == nil {
		hits = []Hit{}
	}
	return hits, allCached, nil
}

// RepoURL is where a person accepts a gated repository's terms.
func RepoURL(repo string) string { return PublicEndpoint + "/" + repo }
