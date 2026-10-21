package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
)

// Former built-in entries (TS src/catalog/former, #368): models earlier
// releases shipped in the built-in catalog. They are not offered for
// browsing or new installs. An install that has one (its primary file on
// disk, or its id in the model settings) gets the shipped spec written to
// catalog.d as a user entry (AdoptFormerEntries), so it keeps its id,
// files and checksums.

type formerEntry struct {
	data []byte // the shipped file, unchanged
	spec *Spec
}

var formerOnce = sync.OnceValue(func() []formerEntry {
	order, err := builtInFS.ReadFile("data/former/order.txt")
	if err != nil {
		panic(err)
	}
	var out []formerEntry
	for _, rel := range strings.Fields(string(order)) {
		b, err := builtInFS.ReadFile("data/former/" + rel)
		if err != nil {
			panic(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			panic(fmt.Sprintf("former %s: %v", rel, err))
		}
		s, err := decodeStrict(m)
		if err != nil {
			panic(fmt.Sprintf("former %s: %v", rel, err))
		}
		s.Trust, s.Origin, s.Tier = "qualified", "built-in", "built-in"
		out = append(out, formerEntry{data: b, spec: s})
	}
	return out
})

// FormerEntry is the spec an earlier release shipped for modelID, or nil
// (TS formerCatalogEntry).
func FormerEntry(modelID string) *Spec {
	for _, e := range formerOnce() {
		if e.spec.ModelID == modelID {
			return e.spec
		}
	}
	return nil
}

// FormerIDs lists every former built-in id.
func FormerIDs() []string {
	var ids []string
	for _, e := range formerOnce() {
		ids = append(ids, e.spec.ModelID)
	}
	return ids
}

// FormerEntryHint opens the notes of a kept former entry (TS FORMER_ENTRY_HINT).
const FormerEntryHint = "No longer in the LocalBase catalog; kept from an earlier release."

// FormerAdoptionLogPath is `<root>/catalog.d/.former-built-ins`: ids already
// kept as user entries, one per line. An id listed there is never written
// again, so removing the user entry sticks.
func FormerAdoptionLogPath(rootDir string) string {
	return filepath.Join(rootDir, "catalog.d", ".former-built-ins")
}

func readAdoptionLog(rootDir string) map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(FormerAdoptionLogPath(rootDir))
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out[l] = true
		}
	}
	return out
}

// formerUserEntry is the file written for a kept entry: the shipped data
// with the hint in front of its notes (TS formerUserEntrySpec).
func formerUserEntry(data []byte) (*jsjson.Object, error) {
	v, err := jsjson.Parse(data)
	if err != nil {
		return nil, err
	}
	o, ok := v.(*jsjson.Object)
	if !ok {
		return nil, fmt.Errorf("not an object")
	}
	notes, _ := o.Get("notes")
	if s, ok := notes.(string); ok && s != "" {
		o.Set("notes", FormerEntryHint+" "+s)
	} else {
		o.Set("notes", FormerEntryHint)
	}
	return o, nil
}

// userPatchAt is the patch a user file at path applies to modelID, or nil
// when the file is a complete spec of its own (or about another model) and
// must be left alone (TS userPatchAt).
func userPatchAt(path, modelID string) (*jsjson.Object, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	v, err := jsjson.Parse(b)
	if err != nil {
		return nil, false
	}
	o, ok := v.(*jsjson.Object)
	if !ok {
		return nil, false
	}
	if id, _ := o.Get("modelId"); id != modelID {
		return nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return nil, false
	}
	if _, err := decodeStrict(m); err == nil {
		return nil, false
	}
	return o, true
}

// AdoptFormerEntries keeps installed or referenced former entries working
// after an upgrade that dropped them (TS adoptFormerCatalogEntries). Each
// former entry whose primary file is under `<root>/models/<kind>/`, or whose
// id is in referenced, is written to `catalog.d/<modelId>.json`; a patch the
// user kept at that path is applied on top. Ids a complete user file already
// provides, and ids kept once before, are skipped. Errors leave the entries
// out. Returns the ids written.
func AdoptFormerEntries(rootDir string, referenced []string) []string {
	var written []string
	var adopted map[string]bool
	for _, e := range formerOnce() {
		id := e.spec.ModelID
		if !slices.Contains(referenced, id) && !formerInstalled(rootDir, e.spec) {
			continue
		}
		path, err := UserEntryPath(rootDir, id)
		if err != nil {
			return written
		}
		var patch *jsjson.Object
		if _, err := os.Stat(path); err == nil {
			p, ok := userPatchAt(path, id)
			if !ok {
				continue
			}
			patch = p
		}
		if adopted == nil {
			adopted = readAdoptionLog(rootDir)
		}
		if adopted[id] {
			continue
		}
		spec, err := formerUserEntry(e.data)
		if err != nil {
			continue
		}
		if patch != nil {
			for _, k := range patch.Keys() {
				v, _ := patch.Get(k)
				spec.Set(k, v)
			}
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return written
		}
		tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
		if err := os.WriteFile(tmp, append(jsjson.Indent(spec), '\n'), 0o600); err != nil {
			return written
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
			return written
		}
		adopted[id] = true
		written = append(written, id)
	}
	if len(written) > 0 {
		ids := make([]string, 0, len(adopted))
		for id := range adopted {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		os.WriteFile(FormerAdoptionLogPath(rootDir), []byte(strings.Join(ids, "\n")+"\n"), 0o600)
	}
	return written
}

// formerInstalled: the entry's primary file is on disk.
func formerInstalled(rootDir string, s *Spec) bool {
	p := s.Primary()
	if p == nil {
		return false
	}
	_, err := os.Stat(filepath.Join(rootDir, "models", s.Kind, p.Filename))
	return err == nil
}

// MissingFormerIDs lists the former entries among ids that known lacks
// (TS missingFormerModelIds).
func MissingFormerIDs(ids []string, known func(string) bool) []string {
	var out []string
	for _, id := range ids {
		if id == "" || slices.Contains(out, id) {
			continue
		}
		if FormerEntry(id) != nil && !known(id) {
			out = append(out, id)
		}
	}
	return out
}

// supersededByBuiltIn: a complete user spec for a built-in id that pins the
// built-in's repository, revision and files exactly (a model added before
// it joined the built-in catalog). It is set aside so the built-in entry
// applies as shipped (TS supersededByBuiltIn).
func supersededByBuiltIn(builtIn, user map[string]json.RawMessage) bool {
	if _, err := decodeStrict(user); err != nil {
		return false
	}
	str := func(m map[string]json.RawMessage, k string) any {
		var v any
		json.Unmarshal(m[k], &v)
		if s, ok := v.(string); ok && k == "source" {
			return strings.TrimRight(s, "/")
		}
		return v
	}
	for _, k := range []string{"source", "repositoryRevision", "kind"} {
		a, _ := json.Marshal(str(builtIn, k))
		b, _ := json.Marshal(str(user, k))
		if string(a) != string(b) {
			return false
		}
	}
	return slices.Equal(pinnedFiles(builtIn["artifacts"]), pinnedFiles(user["artifacts"]))
}

// pinnedFiles: the fields that pin each artifact's download, canonical and
// sorted (TS artifactPin).
func pinnedFiles(raw json.RawMessage) []string {
	var arts []map[string]any
	if json.Unmarshal(raw, &arts) != nil {
		return nil
	}
	var out []string
	for _, a := range arts {
		pin := map[string]any{}
		for _, k := range []string{"sourcePath", "filename", "expectedSizeBytes", "sha256", "source"} {
			if v, ok := a[k]; ok {
				if s, ok := v.(string); ok && k == "sha256" {
					v = strings.ToLower(s)
				}
				pin[k] = v
			}
		}
		b, _ := json.Marshal(pin) // map keys sort: the TS canonical form
		out = append(out, string(b))
	}
	sort.Strings(out)
	return out
}

// UsesReferenceMeasurements: checked-in memory records apply to qualified
// entries, and to a former entry kept as a user entry while every
// launch-shaped field matches what was measured (TS usesReferenceMeasurements).
func (s *Spec) UsesReferenceMeasurements() bool {
	if s.Qualified() {
		return true
	}
	former := FormerEntry(s.ModelID)
	if former == nil {
		return false
	}
	a, b := specFields(former), specFields(s)
	for k := range launchShaped {
		if string(a[k]) != string(b[k]) {
			return false
		}
	}
	return true
}

// specFields is a spec's top-level JSON fields, each in canonical form.
func specFields(s *Spec) map[string][]byte {
	raw, _ := json.Marshal(s)
	var m map[string]any
	json.Unmarshal(raw, &m)
	out := map[string][]byte{}
	for k, v := range m {
		out[k], _ = json.Marshal(v)
	}
	return out
}
