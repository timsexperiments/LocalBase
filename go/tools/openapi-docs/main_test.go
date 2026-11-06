package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
)

func theDocument(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", documentPath))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func theRender(t *testing.T, raw []byte) []byte {
	t.Helper()
	page, err := render(raw)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func TestPageIsCurrent(t *testing.T) {
	current, err := os.ReadFile(filepath.Join("..", "..", pagePath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, theRender(t, theDocument(t))) {
		t.Fatalf("docs/api-reference.md is not what the OpenAPI document gives. From go/, run: %s", regenerate)
	}
}

// CI compares bytes, so anything that varies between runs would fail for nobody's change.
func TestSameDocumentGivesTheSameBytes(t *testing.T) {
	raw := theDocument(t)
	first := theRender(t, raw)
	for range 20 {
		if !bytes.Equal(first, theRender(t, raw)) {
			t.Fatal("two runs gave different pages")
		}
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	reordered, err := json.MarshalIndent(decoded, "", "    ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, theRender(t, reordered)) {
		t.Fatal("the order of the document's keys changed the page")
	}
}

// section is the page's text for one operation.
func section(t *testing.T, page, heading string) string {
	t.Helper()
	_, rest, ok := strings.Cut(page, heading+"\n")
	if !ok {
		t.Fatalf("the page has no %q", heading)
	}
	if next := strings.Index(rest, "\n## "); next >= 0 {
		rest = rest[:next]
	}
	if next := strings.Index(rest, "\n### "); next >= 0 {
		rest = rest[:next]
	}
	return rest
}

// Nothing the document says about an operation may be left off without this failing: not one
// operation, permission, parameter, request field or response code.
func TestEveryOperationIsOnThePage(t *testing.T) {
	raw := theDocument(t)
	page := string(theRender(t, raw))
	var root object
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	d := document{root}
	total := 0
	for path, v := range asObject(root["paths"]) {
		item := asObject(v)
		for key := range item {
			if key == "parameters" || key == "summary" || key == "description" {
				continue
			}
			if !slices.Contains(methods, key) {
				t.Errorf("%s has a %q the page leaves out", path, key)
				continue
			}
			total++
			op := asObject(item[key])
			text := section(t, page, fmt.Sprintf("### `%s %s`", strings.ToUpper(key), path))
			if perm := str(op, "x-localbase-permission"); perm != "" && !strings.Contains(text, "`"+perm+"`") {
				t.Errorf("%s %s: no permission %s", key, path, perm)
			}
			for code := range asObject(op["responses"]) {
				if !strings.Contains(text, "- `"+code+"`") {
					t.Errorf("%s %s: no response %s", key, path, code)
				}
			}
			for _, source := range []object{item, op} {
				list, _ := source["parameters"].([]any)
				for _, p := range list {
					if name := str(d.resolve(asObject(p)), "name"); !strings.Contains(text, "`"+name+"`") {
						t.Errorf("%s %s: no parameter %s", key, path, name)
					}
				}
			}
			content := asObject(d.resolve(asObject(op["requestBody"]))["content"])
			for _, media := range content {
				schema := d.resolve(asObject(asObject(media)["schema"]))
				shapes := []object{schema}
				if variants, ok := schema["oneOf"].([]any); ok {
					shapes = nil
					for _, v := range variants {
						shapes = append(shapes, d.resolve(asObject(v)))
					}
				}
				for _, shape := range shapes {
					for field := range asObject(shape["properties"]) {
						if !strings.Contains(text, "`"+field) {
							t.Errorf("%s %s: no request field %s", key, path, field)
						}
					}
				}
			}
		}
	}
	if got := strings.Count(page, "\n### `"); got != total {
		t.Errorf("the page has %d operations, the document %d", got, total)
	}
	if total == 0 {
		t.Fatal("the document has no operations, so this proved nothing")
	}
}

func TestEveryErrorCodeIsOnThePage(t *testing.T) {
	raw := theDocument(t)
	page := string(theRender(t, raw))
	var root object
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	list, _ := asObject(asObject(asObject(root["components"])["schemas"])["ErrorCode"])["enum"].([]any)
	if len(list) == 0 {
		t.Fatal("the document has no ErrorCode list, so this proved nothing")
	}
	_, section, ok := strings.Cut(page, "## Error codes\n")
	if !ok {
		t.Fatal("the page has no error codes")
	}
	for _, c := range list {
		if !strings.Contains(section, fmt.Sprintf("- `%v`\n", c)) {
			t.Errorf("%v isn't on the page", c)
		}
	}
}

// Generic copy names no models, so a page generated from a document can't either.
func TestPageNamesNoCatalogModel(t *testing.T) {
	page := string(theRender(t, theDocument(t)))
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	specs := cat.All()
	if len(specs) == 0 {
		t.Fatal("no catalog, so this proved nothing")
	}
	for _, s := range specs {
		if strings.Contains(page, s.ModelID) {
			t.Errorf("the page names %s", s.ModelID)
		}
	}
}

func TestSmallDocumentRendersAsExpected(t *testing.T) {
	// A small document, so a mistake in the tables shows as text rather than a missing line.
	page := string(theRender(t, []byte(`{
		"tags": [{"name": "T", "description": "About T."}],
		"paths": {"/x/{id}": {
			"parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}, "description": "Which | one."}],
			"post": {"tags": ["T"], "summary": "Do it", "x-localbase-permission": "x:write",
				"parameters": [{"name": "limit", "in": "query", "schema": {"type": "integer"}}],
				"requestBody": {"content": {"application/json": {"schema": {"oneOf": [
					{"properties": {"action": {"const": "go"}, "to": {"type": "string"}}, "required": ["to"]},
					{"properties": {"action": {"const": "stop"}}}
				]}}}},
				"responses": {"200": {"description": "Done."}, "404": {"description": "No such thing."}}
			}
		}}
	}`)))
	for _, want := range []string{
		"## T\n\nAbout T.\n",
		"### `POST /x/{id}`\n\nDo it.\n\n**Needs:** `x:write`.\n",
		"| `limit` | query | integer |  |  |\n",
		"| `id` | path | string | yes | Which \\| one. |\n",
		"- `action: go`: `to` (required)\n",
		"- `action: stop`\n",
		"- `200`: Done.\n",
		"- `404`: No such thing.\n",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q in:\n%s", want, page)
		}
	}
}
