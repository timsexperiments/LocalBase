package catalog

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
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
)

// The remote catalog feed (TS catalog/remote-feed.ts): the project publishes
// its catalog as a feed a gateway downloads about once a day, so new models
// arrive without a release. The document is checked as a whole (format,
// version, a sha256 of its body), then each entry like any catalog entry.
// The last good download under <root>/cache/catalog-feed/ is what every
// process loads; a failed refresh keeps it.

const (
	FeedFormat         = "localbase-catalog-feed"
	FeedSchemaVersion  = 2
	FeedLegacyVersion  = 1
	DefaultFeedURL     = "https://raw.githubusercontent.com/timsexperiments/LocalBase/main/catalog/feed-v2.json"
	FeedTTL            = 24 * time.Hour
	FeedURLEnv         = "LOCALBASE_CATALOG_FEED_URL"
	FeedCheckInterval  = time.Hour
	maxFeedBytes       = 8 * 1024 * 1024
	maxFeedModels      = 5_000
	feedRequestTimeout = 20 * time.Second
)

var feedSHARe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// FeedURLProblem is why a feed URL is unacceptable ("" when fine): feeds are
// HTTPS; plain HTTP only on the loopback interface.
func FeedURLProblem(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || !whatwgURL(raw) {
		return "is not a URL"
	}
	if u.User != nil {
		if pw, _ := u.User.Password(); u.User.Username() != "" || pw != "" {
			return "must not contain credentials"
		}
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return ""
	case "http":
		host := strings.ToLower(u.Hostname())
		if host == "localhost" || host == "127.0.0.1" || (host == "::1" && strings.HasPrefix(u.Host, "[")) {
			return ""
		}
	}
	return "must use https:// (plain http:// is allowed only for localhost)"
}

// Feed is a parsed, verified feed document. Raw keeps the document for the
// cache; Models and AnnotationItems are its arrays.
type Feed struct {
	SchemaVersion   int
	SHA256          string
	Models          []json.RawMessage
	AnnotationItems []any // jsjson values; nil without the key
	Raw             json.RawMessage
}

// FeedError is a rejected download or document.
type FeedError struct{ Message string }

func (e *FeedError) Error() string { return e.Message }

var feedHeaderKeys = map[string]bool{"format": true, "schemaVersion": true, "minReaderVersion": true, "sha256": true}

// feedChecksum is the sha256 of JSON.stringify of the models (v1) or of the
// body keys, sorted (v2 and later).
func feedChecksum(version int, doc *jsjson.Object) string {
	var payload []byte
	if version == FeedLegacyVersion {
		models, _ := doc.Get("models")
		payload = jsjson.Stringify(models)
	} else {
		var keys []string
		for _, k := range doc.Keys() {
			if !feedHeaderKeys[k] {
				keys = append(keys, k)
			}
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		body := jsjson.NewObject()
		for _, k := range keys {
			v, _ := doc.Get(k)
			body.Set(k, v)
		}
		payload = jsjson.Stringify(body)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// utf16Less is JavaScript's default string sort (UTF-16 code units).
func utf16Less(a, b string) bool {
	ua, ub := []rune(a), []rune(b)
	ea, eb := encode16(ua), encode16(ub)
	for i := 0; i < len(ea) && i < len(eb); i++ {
		if ea[i] != eb[i] {
			return ea[i] < eb[i]
		}
	}
	return len(ea) < len(eb)
}

func encode16(r []rune) []uint16 {
	var out []uint16
	for _, c := range r {
		if c >= 0x10000 {
			c -= 0x10000
			out = append(out, uint16(0xd800+(c>>10)), uint16(0xdc00+(c&0x3ff)))
		} else {
			out = append(out, uint16(c))
		}
	}
	return out
}

func positiveInt(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || f <= 0 || f > 1<<53-1 {
		return 0, false
	}
	return int(f), true
}

func readableVersion(schema int, minReader int, hasMin bool) bool {
	if schema <= FeedSchemaVersion {
		return true
	}
	return hasMin && minReader <= FeedSchemaVersion
}

func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case *jsjson.Object:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	}
	return "undefined"
}

// ParseFeed parses and checks a downloaded feed document (TS
// parseCatalogFeed, with its messages).
func ParseFeed(text []byte) (*Feed, error) {
	v, err := jsjson.Parse(text)
	if err != nil {
		return nil, &FeedError{"The feed is not JSON."}
	}
	notFeed := &FeedError{`The document is not a LocalBase catalog feed (expected "format": "` + FeedFormat + `").`}
	doc, ok := v.(*jsjson.Object)
	if !ok {
		return nil, notFeed
	}
	format, _ := doc.Get("format")
	sv, _ := doc.Get("schemaVersion")
	schema, okSchema := positiveInt(sv)
	mv, hasMin := doc.Get("minReaderVersion")
	minReader, okMin := positiveInt(mv)
	if format != FeedFormat || !okSchema || (hasMin && !okMin) {
		return nil, notFeed
	}
	if !readableVersion(schema, minReader, hasMin) {
		return nil, &FeedError{fmt.Sprintf("The feed uses schema version %d; this LocalBase reads up to version %d. Upgrade LocalBase to use it.", schema, FeedSchemaVersion)}
	}
	malformed := func(path, msg string) error { return &FeedError{"The feed is malformed (" + path + ": " + msg + ")."} }
	shaV, hasSHA := doc.Get("sha256")
	shaStr, isStr := shaV.(string)
	switch {
	case !hasSHA || !isStr:
		return nil, malformed("sha256", "Invalid input: expected string, received "+kindIfPresent(shaV, hasSHA))
	case !feedSHARe.MatchString(shaStr):
		return nil, malformed("sha256", "Invalid string: must match pattern /^[0-9a-f]{64}$/")
	}
	mvv, hasModels := doc.Get("models")
	models, isArr := mvv.([]any)
	switch {
	case !hasModels || !isArr:
		return nil, malformed("models", "Invalid input: expected array, received "+kindIfPresent(mvv, hasModels))
	case len(models) > maxFeedModels:
		return nil, malformed("models", fmt.Sprintf("Too big: expected array to have <=%d items", maxFeedModels))
	}
	var annotations []any
	if av, has := doc.Get("annotations"); has {
		arr, ok := av.([]any)
		switch {
		case !ok:
			return nil, malformed("annotations", "Invalid input: expected array, received "+kindOf(av))
		case len(arr) > MaxFeedAnnotations:
			return nil, malformed("annotations", fmt.Sprintf("Too big: expected array to have <=%d items", MaxFeedAnnotations))
		}
		annotations = arr
	}
	if feedChecksum(schema, doc) != shaStr {
		return nil, &FeedError{"The feed's sha256 does not match its contents; it was truncated or altered."}
	}
	f := &Feed{SchemaVersion: schema, SHA256: shaStr, AnnotationItems: annotations, Raw: jsjson.Stringify(doc)}
	for _, m := range models {
		f.Models = append(f.Models, jsjson.Stringify(m))
	}
	return f, nil
}

func kindIfPresent(v any, present bool) string {
	if !present {
		return "undefined"
	}
	return kindOf(v)
}

// --- settings ---------------------------------------------------------------

// FeedSettings is the feed URL in force: environment, then the root's
// settings, then the default. URL "" means off.
type FeedSettings struct {
	URL     string
	Source  string // default | settings | environment
	Problem string
}

func FeedSettingsPath(rootDir string) string { return filepath.Join(rootDir, "catalog-feed.json") }

// ReadFeedSettings resolves the feed URL for a root.
func ReadFeedSettings(rootDir string, getenv func(string) string) FeedSettings {
	if getenv == nil {
		getenv = os.Getenv
	}
	if env := strings.TrimSpace(getenv(FeedURLEnv)); env != "" {
		if env == "off" {
			return FeedSettings{Source: "environment"}
		}
		if p := FeedURLProblem(env); p != "" {
			return FeedSettings{Source: "environment", Problem: FeedURLEnv + " " + p + "; the feed is off."}
		}
		return FeedSettings{URL: env, Source: "environment"}
	}
	path := FeedSettingsPath(rootDir)
	b, err := os.ReadFile(path)
	if err != nil {
		return FeedSettings{URL: DefaultFeedURL, Source: "default"}
	}
	invalid := func(why string) FeedSettings {
		return FeedSettings{URL: DefaultFeedURL, Source: "default", Problem: path + " is invalid (" + why + "); using the default feed."}
	}
	v, err := jsjson.Parse(b)
	if err != nil {
		return invalid("JSON Parse error: Unable to parse JSON string")
	}
	o, ok := v.(*jsjson.Object)
	if !ok {
		return invalid("Invalid input: expected object, received " + kindOf(v))
	}
	for _, k := range o.Keys() {
		if k != "url" {
			return invalid(`Unrecognized key: "` + k + `"`)
		}
	}
	u, present := o.Get("url")
	switch x := u.(type) {
	case nil:
		if !present {
			return invalid("Invalid input: expected string, received undefined")
		}
		return FeedSettings{Source: "settings"}
	case string:
		if x == "" {
			return invalid("Too small: expected string to have >=1 characters")
		}
		if p := FeedURLProblem(x); p != "" {
			return invalid(p)
		}
		return FeedSettings{URL: x, Source: "settings"}
	}
	return invalid("Invalid input: expected string, received " + kindOf(u))
}

// WriteFeedSettings saves a URL, "" to turn the feed off, or "default".
func WriteFeedSettings(rootDir, url string) error {
	path := FeedSettingsPath(rootDir)
	if url == "default" {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var value any
	if url != "" {
		if p := FeedURLProblem(url); p != "" {
			return &FeedError{p}
		}
		value = url
	}
	b, _ := json.MarshalIndent(map[string]any{"url": value}, "", "  ")
	return writePrivate(path, append(b, '\n'))
}

func writePrivate(path string, b []byte) error {
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- cache ------------------------------------------------------------------

func FeedCacheDir(rootDir string) string { return filepath.Join(rootDir, "cache", "catalog-feed") }

type cachedFeed struct {
	URL       string          `json:"url"`
	FetchedAt string          `json:"fetchedAt"`
	ETag      *string         `json:"etag"`
	Feed      json.RawMessage `json:"feed"`
}

type feedState struct {
	URL       string  `json:"url"`
	CheckedAt string  `json:"checkedAt"`
	LastError *string `json:"lastError"`
}

var isoRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)

// readCached is the cached feed for url, re-parsed (without the checksum
// check, as TS reads its cache) and still readable by this version.
func readCached(rootDir, url string) (*cachedFeed, *Feed) {
	b, err := os.ReadFile(filepath.Join(FeedCacheDir(rootDir), "feed.json"))
	if err != nil {
		return nil, nil
	}
	var c cachedFeed
	if json.Unmarshal(b, &c) != nil || c.URL != url || !isoRe.MatchString(c.FetchedAt) {
		return nil, nil
	}
	f, err := parseCachedFeed(c.Feed)
	if err != nil {
		return nil, nil
	}
	return &c, f
}

// parseCachedFeed accepts a cached document: the same checks as a
// download except the checksum, which was verified when it was stored.
func parseCachedFeed(raw json.RawMessage) (*Feed, error) {
	v, err := jsjson.Parse(raw)
	if err != nil {
		return nil, err
	}
	doc, ok := v.(*jsjson.Object)
	if !ok {
		return nil, errors.New("not an object")
	}
	sv, _ := doc.Get("schemaVersion")
	schema, ok := positiveInt(sv)
	mv, hasMin := doc.Get("minReaderVersion")
	minReader, _ := positiveInt(mv)
	if !ok || !readableVersion(schema, minReader, hasMin) {
		return nil, errors.New("unreadable version")
	}
	shaV, _ := doc.Get("sha256")
	sha, _ := shaV.(string)
	models, ok := func() ([]any, bool) { m, _ := doc.Get("models"); a, ok := m.([]any); return a, ok }()
	if !ok || !feedSHARe.MatchString(sha) {
		return nil, errors.New("malformed")
	}
	f := &Feed{SchemaVersion: schema, SHA256: sha, Raw: raw}
	if av, has := doc.Get("annotations"); has {
		f.AnnotationItems, _ = av.([]any)
	}
	for _, m := range models {
		f.Models = append(f.Models, jsjson.Stringify(m))
	}
	return f, nil
}

func readState(rootDir, url string) *feedState {
	b, err := os.ReadFile(filepath.Join(FeedCacheDir(rootDir), "state.json"))
	if err != nil {
		return nil
	}
	var s feedState
	if json.Unmarshal(b, &s) != nil || s.URL != url || !isoRe.MatchString(s.CheckedAt) {
		return nil
	}
	return &s
}

func isoNow(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func writeJSONLine(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writePrivate(path, append(b, '\n'))
}

// builtInIDs are the model ids this binary ships or an earlier release
// shipped (former entries, #368); a feed never provides them. The published
// feed keeps former entries for older binaries.
var builtInIDs = func() map[string]bool {
	out := map[string]bool{}
	for _, id := range FormerIDs() {
		out[id] = true
	}
	order, _ := builtInFS.ReadFile("data/order.txt")
	for _, rel := range strings.Fields(string(order)) {
		b, _ := builtInFS.ReadFile("data/" + rel)
		var m struct{ ModelID string }
		json.Unmarshal(b, &m)
		out[m.ModelID] = true
	}
	return out
}()

func entryID(raw json.RawMessage) string {
	var m struct {
		ModelID any `json:"modelId"`
	}
	json.Unmarshal(raw, &m)
	s, _ := m.ModelID.(string)
	return s
}

// ReadRemoteLayer is the remote layer from the last good download, or nil
// when the feed is off or nothing was downloaded. It never uses the network.
func ReadRemoteLayer(rootDir string, getenv func(string) string) *RemoteLayer {
	s := ReadFeedSettings(rootDir, getenv)
	if s.URL == "" {
		return nil
	}
	_, f := readCached(rootDir, s.URL)
	if f == nil {
		return nil
	}
	layer := &RemoteLayer{URL: s.URL}
	for _, m := range f.Models {
		if id := entryID(m); id == "" || !builtInIDs[id] {
			layer.Entries = append(layer.Entries, m)
		}
	}
	layer.Annotations, _ = ParseAnnotations(f.AnnotationItems)
	return layer
}

// LoadRoot loads the effective catalog of a root: built-in, the cached
// remote feed and catalog.d. Installed former entries are kept as user
// entries first (TS loadCatalogSources).
func LoadRoot(rootDir string, getenv func(string) string) (*Catalog, error) {
	AdoptFormerEntries(rootDir, nil)
	return LoadLayers(filepath.Join(rootDir, "catalog.d"), ReadRemoteLayer(rootDir, getenv))
}

// --- refresh ----------------------------------------------------------------

type RefreshOptions struct {
	Force  bool
	Client *http.Client
	Now    func() time.Time
	Getenv func(string) string
}

type RefreshResult struct {
	Outcome string `json:"outcome"` // disabled | fresh | unchanged | updated | failed
	URL     string `json:"-"`
	Error   string `json:"error,omitempty"`
}

// RefreshFeed downloads the feed when the cached copy is older than the TTL
// (or always with Force), validates it and replaces the cache. On failure
// the last good copy stays and the error is recorded.
func RefreshFeed(ctx context.Context, rootDir string, o RefreshOptions) RefreshResult {
	now := time.Now()
	if o.Now != nil {
		now = o.Now()
	}
	s := ReadFeedSettings(rootDir, o.Getenv)
	if s.URL == "" {
		return RefreshResult{Outcome: "disabled"}
	}
	cached, _ := readCached(rootDir, s.URL)
	state := readState(rootDir, s.URL)
	lastCheck := ""
	if state != nil {
		lastCheck = state.CheckedAt
	} else if cached != nil {
		lastCheck = cached.FetchedAt
	}
	if t, err := time.Parse(time.RFC3339Nano, lastCheck); !o.Force && cached != nil && err == nil && now.Sub(t) < FeedTTL {
		return RefreshResult{Outcome: "fresh", URL: s.URL}
	}
	// Never create files in a directory that is not a LocalBase root yet.
	_, err := os.Stat(filepath.Join(rootDir, ".localbase-root.json"))
	writable := err == nil
	dir := FeedCacheDir(rootDir)
	record := func(lastError string) {
		if !writable {
			return
		}
		os.MkdirAll(dir, 0o700)
		st := feedState{URL: s.URL, CheckedAt: isoNow(now)}
		if lastError != "" {
			st.LastError = &lastError
		}
		writeJSONLine(filepath.Join(dir, "state.json"), st)
	}
	fail := func(msg string) RefreshResult {
		record(msg)
		return RefreshResult{Outcome: "failed", URL: s.URL, Error: msg}
	}
	client := o.Client
	if client == nil {
		client = &http.Client{}
	}
	ctx, cancel := context.WithTimeout(ctx, feedRequestTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	req.Header.Set("Accept", "application/json")
	if cached != nil && cached.ETag != nil {
		req.Header.Set("If-None-Match", *cached.ETag)
	}
	var redirectProblem string
	c := *client
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 20 {
			return errors.New("stopped after 20 redirects")
		}
		if p := FeedURLProblem(r.URL.String()); p != "" {
			redirectProblem = p
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, err := c.Do(req)
	if redirectProblem != "" {
		if resp != nil {
			resp.Body.Close()
		}
		return fail("The feed redirected to a URL that " + redirectProblem + ".")
	}
	if err != nil {
		return fail("Could not reach the feed (" + errMessage(err) + ").")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && cached != nil {
		record("")
		return RefreshResult{Outcome: "unchanged", URL: s.URL}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fail(fmt.Sprintf("The feed server answered %d.", resp.StatusCode))
	}
	if resp.ContentLength > maxFeedBytes {
		return fail("The feed is larger than 8 MiB.")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		return fail("The feed download failed (" + errMessage(err) + ").")
	}
	if len(body) > maxFeedBytes {
		return fail("The feed is larger than 8 MiB.")
	}
	feed, err := ParseFeed(body)
	if err != nil {
		var fe *FeedError
		if errors.As(err, &fe) {
			return fail(fe.Message)
		}
		return fail("The feed download failed (" + err.Error() + ").")
	}
	_, prev := readCached(rootDir, s.URL)
	unchanged := prev != nil && prev.SHA256 == feed.SHA256
	if writable {
		os.MkdirAll(dir, 0o700)
		var etag *string
		if e := resp.Header.Get("ETag"); e != "" {
			etag = &e
		}
		if err := writeJSONLine(filepath.Join(dir, "feed.json"), cachedFeed{URL: s.URL, FetchedAt: isoNow(now), ETag: etag, Feed: feed.Raw}); err != nil {
			return fail("The feed could not be cached (" + err.Error() + ").")
		}
	}
	record("")
	if unchanged {
		return RefreshResult{Outcome: "unchanged", URL: s.URL}
	}
	return RefreshResult{Outcome: "updated", URL: s.URL}
}

func errMessage(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// --- status -----------------------------------------------------------------

// FeedStatus describes the feed for a root (TS CatalogFeedStatus).
type FeedStatus struct {
	Enabled            bool     `json:"enabled"`
	URL                *string  `json:"url"`
	Source             string   `json:"source"`
	Problem            *string  `json:"problem"`
	FetchedAt          *string  `json:"fetchedAt"`
	CheckedAt          *string  `json:"checkedAt"`
	Revision           *string  `json:"revision"`
	Stale              bool     `json:"stale"`
	LastError          *string  `json:"lastError"`
	Entries            int      `json:"entries"`
	BuiltIn            int      `json:"builtIn"`
	Added              []string `json:"added"`
	Skipped            int      `json:"skipped"`
	SchemaVersion      *int     `json:"schemaVersion"`
	Annotations        int      `json:"annotations"`
	AnnotationsSkipped int      `json:"annotationsSkipped"`
}

// Status describes the feed; cat is the effective catalog for the root.
func Status(rootDir string, cat *Catalog, getenv func(string) string, now time.Time) FeedStatus {
	s := ReadFeedSettings(rootDir, getenv)
	st := FeedStatus{Source: s.Source, Added: []string{}}
	if s.Problem != "" {
		st.Problem = &s.Problem
	}
	if s.URL == "" {
		return st
	}
	st.Enabled, st.URL = true, &s.URL
	cached, f := readCached(rootDir, s.URL)
	state := readState(rootDir, s.URL)
	var checked string
	switch {
	case state != nil:
		checked = state.CheckedAt
	case cached != nil:
		checked = cached.FetchedAt
	}
	if cached != nil {
		st.FetchedAt = &cached.FetchedAt
		st.Revision = &f.SHA256
		st.Entries = len(f.Models)
		v := f.SchemaVersion
		st.SchemaVersion = &v
		for _, m := range f.Models {
			if id := entryID(m); id != "" && builtInIDs[id] {
				st.BuiltIn++
			}
		}
		ann, skipped := ParseAnnotations(f.AnnotationItems)
		st.Annotations, st.AnnotationsSkipped = len(ann), skipped
	}
	if checked != "" {
		st.CheckedAt = &checked
	}
	t, err := time.Parse(time.RFC3339Nano, checked)
	st.Stale = cached == nil || err != nil || now.Sub(t) >= FeedTTL
	if state != nil {
		st.LastError = state.LastError
	}
	if cat != nil {
		for _, m := range cat.All() {
			if m.Origin == "remote" {
				st.Added = append(st.Added, m.ModelID)
			}
		}
		for _, d := range cat.Diagnostics {
			if d.Tier == "remote" {
				st.Skipped++
			}
		}
	}
	return st
}
