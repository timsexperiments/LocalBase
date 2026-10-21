package conf

import (
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/toml"
)

// Declarative configuration: the versioned TOML document `config
// validate/plan/apply/show` read and write (TS declarative.ts). It holds the
// listener, runtime, model and memory settings; secrets, observability and
// process overrides are not part of it and are preserved on apply.

// DocumentError is an invalid document; the CLI reports it as bad input.
type DocumentError struct{ Message string }

func (e *DocumentError) Error() string { return e.Message }

// ParseDocument validates a TOML document as the TS zod schema does and
// returns its settings: a configuration with only the declarative fields
// set, selections sorted.
func ParseDocument(text string, cat *catalog.Catalog) (*store.Config, error) {
	doc, err := toml.ParseDocument(text)
	if err != nil {
		// Parser diagnostics can contain source text, including pasted secrets.
		return nil, &DocumentError{"Invalid TOML configuration."}
	}
	v := &docValidator{doc: doc, cat: cat}
	c := v.document(doc.Root)
	if len(v.issues) > 0 {
		return nil, &DocumentError{Summary(v.issues)}
	}
	normalize(c)
	return c, nil
}

func normalize(c *store.Config) {
	for _, k := range store.Kinds {
		ids := slices.Clone(c.Selected[k])
		if ids == nil {
			ids = []string{}
		}
		sort.Strings(ids)
		c.Selected[k] = ids
	}
}

// Document is the declarative view of a configuration, in the TS schema's
// key order, selections sorted.
func Document(c *store.Config) *jsjson.Object {
	n := Project(c)
	normalize(n)
	strs := func(ids []string) []any {
		out := make([]any, len(ids))
		for i, id := range ids {
			out[i] = id
		}
		return out
	}
	obj := func(kv ...any) *jsjson.Object {
		o := jsjson.NewObject()
		for i := 0; i < len(kv); i += 2 {
			o.Set(kv[i].(string), kv[i+1])
		}
		return o
	}
	var parallel any = n.Parallel
	if p, err := strconv.Atoi(n.Parallel); err == nil {
		parallel = float64(p)
	}
	reserve := func(r store.MemoryReserve) *jsjson.Object {
		return obj("percent", r.Percent, "minimumGb", r.MinimumGB)
	}
	return obj(
		"version", float64(1),
		"gateway", obj("host", n.GatewayHost, "port", float64(n.GatewayPort)),
		"runtime", obj("host", n.Host, "port", float64(n.Port), "ctxSize", float64(n.CtxSize), "ctxMode", n.CtxMode,
			"parallel", parallel, "sttHost", n.SttHost, "sttPort", float64(n.SttPort)),
		"models", obj("allowExperimental", n.AllowExperimental,
			"selectedLlmModels", strs(n.Selected["llm"]), "selectedSttModels", strs(n.Selected["stt"]),
			"selectedTtsModels", strs(n.Selected["tts"]), "selectedImageModels", strs(n.Selected["image"]),
			"selectedVideoModels", strs(n.Selected["video"]),
			"activeLlmModel", n.Active["llm"], "activeSttModel", n.Active["stt"], "activeTtsModel", n.Active["tts"],
			"activeImageModel", n.Active["image"], "activeVideoModel", n.Active["video"]),
		"memory", obj("systemReserve", reserve(n.SystemReserve), "acceleratorReserve", reserve(n.AcceleratorReserve)),
	)
}

// Project copies the declarative fields of c.
func Project(c *store.Config) *store.Config {
	n := &store.Config{GatewayHost: c.GatewayHost, GatewayPort: c.GatewayPort, Host: c.Host, Port: c.Port,
		CtxSize: c.CtxSize, CtxMode: c.CtxMode, Parallel: c.Parallel, SttHost: c.SttHost, SttPort: c.SttPort,
		AllowExperimental: c.AllowExperimental, Selected: map[string][]string{}, Active: map[string]string{},
		SystemReserve: c.SystemReserve, AcceleratorReserve: c.AcceleratorReserve}
	for _, k := range store.Kinds {
		n.Selected[k] = slices.Clone(c.Selected[k])
		n.Active[k] = c.Active[k]
	}
	return n
}

// Compose applies a document's settings over current (TS
// composeConfiguration): everything else, secrets included, is kept.
func Compose(current, desired *store.Config) *store.Config {
	next := current.Clone()
	d := Project(desired)
	next.GatewayHost, next.GatewayPort = d.GatewayHost, d.GatewayPort
	next.Host, next.Port, next.CtxSize, next.CtxMode, next.Parallel = d.Host, d.Port, d.CtxSize, d.CtxMode, d.Parallel
	next.SttHost, next.SttPort = d.SttHost, d.SttPort
	next.AllowExperimental, next.Selected, next.Active = d.AllowExperimental, d.Selected, d.Active
	next.SystemReserve, next.AcceleratorReserve = d.SystemReserve, d.AcceleratorReserve
	return next
}

// Change is one planned setting change.
type Change struct {
	Path       string `json:"path"`
	Before     any    `json:"before"`
	After      any    `json:"after"`
	Activation string `json:"activation"`
}

// Plan compares a document with the saved configuration (TS
// configurationPlanSchema).
type Plan struct {
	Changed         bool     `json:"changed"`
	RestartRequired bool     `json:"restartRequired"`
	PendingRestart  bool     `json:"pendingRestart"`
	Changes         []Change `json:"changes"`
}

// PlanChanges diffs desired against current (nil: nothing saved). The
// listener and memory policy need a restart; the rest applies hot. A newly
// selected video model must be eligible on this host.
func PlanChanges(current, desired *store.Config, pending bool, cat *catalog.Catalog) (*Plan, error) {
	var previous []string
	if current != nil {
		previous = current.Selected["video"]
	}
	for _, id := range desired.Selected["video"] {
		if slices.Contains(previous, id) {
			continue
		}
		if s := cat.ByID(id); s != nil {
			if reason := Ineligible(s, desired.AllowExperimental); reason != "" {
				return nil, &DocumentError{id + ": " + reason}
			}
		}
	}
	var before *jsjson.Object
	if current != nil {
		before = Document(current)
	}
	after := Document(desired)
	plan := &Plan{Changes: []Change{}}
	var compare func(path string, left, right any, activation string)
	compare = func(path string, left, right any, activation string) {
		if r, ok := right.(*jsjson.Object); ok {
			l, _ := left.(*jsjson.Object)
			for _, k := range r.Keys() {
				var lv any
				if l != nil {
					lv, _ = l.Get(k)
				}
				rv, _ := r.Get(k)
				compare(path+"."+k, lv, rv, activation)
			}
			return
		}
		if string(jsjson.Stringify(left)) != string(jsjson.Stringify(right)) {
			plan.Changes = append(plan.Changes, Change{path, left, right, activation})
		}
	}
	section := func(name string) (any, any) {
		var l any
		if before != nil {
			l, _ = before.Get(name)
		}
		r, _ := after.Get(name)
		return l, r
	}
	l, r := section("gateway")
	compare("gateway", l, r, "restart-required")
	l, r = section("runtime")
	compare("runtime", l, r, "hot")
	l, r = section("models")
	compare("models", l, r, "hot")
	l, r = section("memory")
	compare("memory", l, r, "restart-required")
	sort.Slice(plan.Changes, func(i, j int) bool { return plan.Changes[i].Path < plan.Changes[j].Path })
	plan.Changed = len(plan.Changes) > 0
	plan.PendingRestart = pending
	plan.RestartRequired = pending || slices.ContainsFunc(plan.Changes, func(c Change) bool { return c.Activation == "restart-required" })
	return plan, nil
}

// Render writes the document as TOML (TS renderConfiguration): scalars
// first in each table, then its subtables, values JSON-encoded.
func Render(c *store.Config) string {
	lines := []string{"# Secrets and process overrides are omitted and preserved on apply.", "version = 1"}
	var table func(path string, o *jsjson.Object)
	table = func(path string, o *jsjson.Object) {
		lines = append(lines, "", "["+path+"]")
		for _, k := range o.Keys() {
			if v, _ := o.Get(k); !isObject(v) {
				lines = append(lines, k+" = "+string(jsjson.Stringify(v)))
			}
		}
		for _, k := range o.Keys() {
			if v, _ := o.Get(k); isObject(v) {
				table(path+"."+k, v.(*jsjson.Object))
			}
		}
	}
	doc := Document(c)
	for _, name := range []string{"gateway", "runtime", "models", "memory"} {
		v, _ := doc.Get(name)
		table(name, v.(*jsjson.Object))
	}
	return strings.Join(lines, "\n") + "\n"
}

func isObject(v any) bool { _, ok := v.(*jsjson.Object); return ok }

// --- validation (the TS zod schema, issue for issue) ---------------------

type docValidator struct {
	doc    *toml.Document
	cat    *catalog.Catalog
	issues []Issue
	// aborted counts issues that stop a parent refinement (zod's invalid
	// type and unrecognized-key issues).
	aborted int
}

func (v *docValidator) add(path []string, message string) {
	p := strings.Join(path, ".")
	if p == "" {
		p = "input"
	}
	v.issues = append(v.issues, Issue{p, message})
}

func (v *docValidator) abort(path []string, message string) {
	v.add(path, message)
	v.aborted++
}

func at(path []string, k ...string) []string { return append(slices.Clone(path), k...) }

func received(x any, present bool) string {
	switch t := x.(type) {
	case nil:
		if !present {
			return "undefined"
		}
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case int64:
		return "number"
	case float64:
		if math.IsNaN(t) {
			return "NaN"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

// keys is a table's keys in JavaScript enumeration order: integer-like
// keys ascending, then the rest as the document wrote them.
func (v *docValidator) keys(m map[string]any) []string {
	o := jsjson.NewObject()
	for _, k := range v.doc.Keys(m) {
		o.Set(k, nil)
	}
	return o.Keys()
}

// object checks a strict object: fn reads the known keys, then extra keys
// are reported. It returns nil if the value is not a table.
func (v *docValidator) object(m map[string]any, path []string, key string, known []string, fn func(o map[string]any, p []string)) {
	p := at(path, key)
	if path == nil && key == "" {
		p = nil
	}
	var x any
	present := true
	if m != nil {
		x, present = m[key]
	}
	if key == "" {
		x = m
	}
	o, ok := x.(map[string]any)
	if !ok {
		v.abort(p, "Invalid input: expected object, received "+received(x, present))
		return
	}
	fn(o, p)
	var extra []string
	for _, k := range v.keys(o) {
		if !slices.Contains(known, k) {
			extra = append(extra, `"`+k+`"`)
		}
	}
	switch len(extra) {
	case 0:
	case 1:
		v.abort(p, "Unrecognized key: "+extra[0])
	default:
		v.abort(p, "Unrecognized keys: "+strings.Join(extra, ", "))
	}
}

func number(x any) (float64, bool) {
	switch n := x.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func numText(f float64) string { return jsjson.Number(f) }

// num is z.number() with .int() when integer, then min and max.
func (v *docValidator) num(o map[string]any, path []string, key string, integer bool, lo, hi float64) float64 {
	p := at(path, key)
	x, present := o[key]
	f, ok := number(x)
	switch {
	case !ok || math.IsNaN(f):
		v.abort(p, "Invalid input: expected number, received "+received(x, present))
		return 0
	case integer && (math.IsInf(f, 0) || f != math.Trunc(f) || math.Abs(f) > 1<<53-1):
		v.abort(p, "Invalid input: expected int, received number")
		return 0
	case math.IsInf(f, 0):
		v.abort(p, "Invalid input: expected number, received number")
		return 0
	case f < lo:
		v.add(p, "Too small: expected number to be >="+numText(lo))
	case !math.IsInf(hi, 1) && f > hi:
		v.add(p, "Too big: expected number to be <="+numText(hi))
	}
	return f
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

func (v *docValidator) str(x any, present bool, p []string) (string, bool) {
	s, ok := x.(string)
	if !ok {
		v.abort(p, "Invalid input: expected string, received "+received(x, present))
	}
	return s, ok
}

func (v *docValidator) host(o map[string]any, path []string, key string) string {
	p := at(path, key)
	x, present := o[key]
	s, ok := v.str(x, present, p)
	if !ok {
		return ""
	}
	if n := utf16Len(s); n < 1 {
		v.add(p, "Too small: expected string to have >=1 characters")
	} else if n > 253 {
		v.add(p, "Too big: expected string to have <=253 characters")
	}
	if s != strings.TrimSpace(s) || strings.ContainsFunc(s, isJSSpace) {
		v.add(p, "must not contain whitespace")
	}
	return s
}

// isJSSpace is JavaScript's \s.
func isJSSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func (v *docValidator) port(o map[string]any, path []string, key string) int {
	return int(v.num(o, path, key, true, 1, 65535))
}

func (v *docValidator) document(root map[string]any) *store.Config {
	c := &store.Config{Selected: map[string][]string{}, Active: map[string]string{}}
	v.object(root, nil, "", []string{"version", "gateway", "runtime", "models", "memory"}, func(o map[string]any, _ []string) {
		if f, ok := number(o["version"]); !ok || f != 1 {
			v.abort([]string{"version"}, "Invalid input: expected 1")
		}
		v.object(o, nil, "gateway", []string{"host", "port"}, func(g map[string]any, p []string) {
			c.GatewayHost = v.host(g, p, "host")
			c.GatewayPort = v.port(g, p, "port")
		})
		v.object(o, nil, "runtime", []string{"host", "port", "ctxSize", "ctxMode", "parallel", "sttHost", "sttPort"},
			func(r map[string]any, p []string) {
				c.Host = v.host(r, p, "host")
				c.Port = v.port(r, p, "port")
				c.CtxSize = int(v.num(r, p, "ctxSize", true, 2048, 2_147_483_647))
				if s, ok := r["ctxMode"].(string); ok && (s == "auto" || s == "explicit") {
					c.CtxMode = s
				} else {
					v.abort(at(p, "ctxMode"), `Invalid option: expected one of "auto"|"explicit"`)
				}
				switch x := r["parallel"].(type) {
				case string:
					if x == "auto" {
						c.Parallel = x
					}
				default:
					if f, ok := number(x); ok && f >= 1 && f <= 4 && f == math.Trunc(f) {
						c.Parallel = strconv.Itoa(int(f))
					}
				}
				if c.Parallel == "" {
					v.abort(at(p, "parallel"), "Invalid input")
				}
				c.SttHost = v.host(r, p, "sttHost")
				c.SttPort = v.port(r, p, "sttPort")
			})
		v.object(o, nil, "models", modelKeys, func(m map[string]any, p []string) { v.models(m, p, c) })
		v.object(o, nil, "memory", []string{"systemReserve", "acceleratorReserve"}, func(m map[string]any, p []string) {
			reserve := func(key string, dst *store.MemoryReserve) {
				v.object(m, p, key, []string{"percent", "minimumGb"}, func(r map[string]any, rp []string) {
					dst.Percent = v.num(r, rp, "percent", false, 0, 100)
					dst.MinimumGB = v.num(r, rp, "minimumGb", false, 0, math.Inf(1))
				})
			}
			reserve("systemReserve", &c.SystemReserve)
			reserve("acceleratorReserve", &c.AcceleratorReserve)
		})
	})
	return c
}

var modelKeys = []string{"allowExperimental", "selectedLlmModels", "selectedSttModels", "selectedTtsModels",
	"selectedImageModels", "selectedVideoModels", "activeLlmModel", "activeSttModel", "activeTtsModel",
	"activeImageModel", "activeVideoModel"}

// models is createModelConfigurationSchema with eligibility checked for
// every kind but video (TS parseConfiguration); the selection refinement
// runs only when the object itself parsed.
func (v *docValidator) models(m map[string]any, p []string, c *store.Config) {
	start := v.aborted
	if x, present := m["allowExperimental"]; present {
		if b, ok := x.(bool); ok {
			c.AllowExperimental = b
		} else {
			v.abort(at(p, "allowExperimental"), "Invalid input: expected boolean, received "+received(x, present))
		}
	}
	for _, k := range store.Kinds {
		field := "selected" + fieldNames[k] + "Models"
		x, present := m[field]
		list, ok := x.([]any)
		if !ok {
			v.abort(at(p, field), "Invalid input: expected array, received "+received(x, present))
			continue
		}
		ids := []string{}
		for i, e := range list {
			ep := at(p, field, strconv.Itoa(i))
			s, ok := v.str(e, true, ep)
			if !ok {
				continue
			}
			if s == "" {
				v.add(ep, "Too small: expected string to have >=1 characters")
			}
			ids = append(ids, s)
		}
		c.Selected[k] = ids
	}
	for _, k := range store.Kinds {
		field := "active" + fieldNames[k] + "Model"
		x, present := m[field]
		s, ok := v.str(x, present, at(p, field))
		if !ok {
			continue
		}
		if k == "llm" && s == "" {
			v.add(at(p, field), "Too small: expected string to have >=1 characters")
		}
		c.Active[k] = s
	}
	// Unrecognized keys are reported by the caller after this returns, and
	// they also stop the refinement: check for them here.
	for _, k := range v.keys(m) {
		if !slices.Contains(modelKeys, k) {
			return
		}
	}
	if v.aborted > start {
		return
	}
	allow := c.AllowExperimental
	for _, k := range store.Kinds {
		field := "selected" + fieldNames[k] + "Models"
		eligibility := k != "video"
		ids := c.Selected[k]
		for i, id := range ids {
			v.modelID(at(p, field, strconv.Itoa(i)), id, k, allow, eligibility)
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				v.add(at(p, field), "must not contain duplicates")
				break
			}
			seen[id] = true
		}
		if k == "llm" && len(ids) == 0 {
			v.add(at(p, field), "Too small: expected array to have >=1 items")
		}
		if eligibility {
			for i, id := range ids {
				if s := v.cat.ByID(id); s != nil {
					if reason := Ineligible(s, allow); reason != "" {
						v.add(at(p, field, strconv.Itoa(i)), reason)
					}
				}
			}
		}
	}
	for _, k := range store.Kinds {
		if id := c.Active[k]; id != "" {
			v.modelID(at(p, "active"+fieldNames[k]+"Model"), id, k, allow, k != "video")
		}
	}
	for _, k := range store.Kinds {
		if id := c.Active[k]; id != "" && !slices.Contains(c.Selected[k], id) {
			v.add(at(p, "active"+fieldNames[k]+"Model"), "must also be present in its selected model list")
		}
	}
}

// modelID is TS modelIdSchema: non-empty, a catalog model of the kind,
// not catalog-only, and eligible.
func (v *docValidator) modelID(p []string, id, kind string, allow, eligibility bool) {
	if id == "" {
		v.add(p, "Too small: expected string to have >=1 characters")
	}
	s := v.cat.ByID(id)
	exp := expectedModalities[kind]
	if s == nil || s.Kind != kind || !slices.Contains(s.InputModalities, exp[0]) || !slices.Contains(s.OutputModalities, exp[1]) {
		v.add(p, "must name a catalog "+kind+" model with compatible modalities")
	}
	if s != nil && s.VideoCatalogOnly {
		v.add(p, "catalog-only models cannot be selected or active because LocalBase inference is unavailable")
	}
	if eligibility && s != nil && Ineligible(s, allow) != "" {
		v.add(p, "model is experimental or unsupported on this platform")
	}
}
