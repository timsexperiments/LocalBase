// Command openapi-docs writes docs/api-reference.md from the OpenAPI document, so the page can't say
// anything the document doesn't. Everything is sorted and nothing depends on when it runs, so the
// same document always gives the same bytes, and CI can compare them.
//
//	go run ./tools/openapi-docs          # from go/: write the page
//	go run ./tools/openapi-docs -check   # say whether it is current
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
)

const (
	documentPath = "internal/gateway/openapi.json"
	pagePath     = "../docs/api-reference.md"
	regenerate   = "go run ./tools/openapi-docs"
)

var methods = []string{"get", "head", "post", "put", "patch", "delete"}

type object = map[string]any

func str(o object, key string) string {
	s, _ := o[key].(string)
	return s
}

// document resolves references within the OpenAPI document.
type document struct{ root object }

func (d document) resolve(o object) object {
	for depth := 0; depth < 8; depth++ {
		ref := str(o, "$ref")
		if ref == "" {
			return o
		}
		var at any = d.root
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, _ := at.(object)
			at = m[part]
		}
		next, ok := at.(object)
		if !ok {
			return o
		}
		o = next
	}
	return o
}

// oneLine makes text safe inside a table cell or a list item.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, "|", `\|`)
}

func refName(o object) string {
	ref := str(o, "$ref")
	return ref[strings.LastIndex(ref, "/")+1:]
}

// typeOf names a schema's type in a few words.
func (d document) typeOf(o object) string {
	if str(o, "$ref") != "" {
		return refName(o)
	}
	if c, ok := o["const"]; ok {
		return fmt.Sprintf("`%v`", c)
	}
	if e, ok := o["enum"].([]any); ok {
		var out []string
		for _, v := range e {
			out = append(out, fmt.Sprintf("`%v`", v))
		}
		return strings.Join(out, ", ")
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		if list, ok := o[key].([]any); ok {
			var out []string
			for _, item := range list {
				if m, ok := item.(object); ok {
					out = append(out, d.typeOf(m))
				}
			}
			return strings.Join(out, " or ")
		}
	}
	name := ""
	switch t := o["type"].(type) {
	case string:
		name = t
	case []any:
		var parts []string
		for _, x := range t {
			parts = append(parts, fmt.Sprint(x))
		}
		name = strings.Join(parts, " or ")
	}
	if strings.HasPrefix(name, "array") {
		if items, ok := o["items"].(object); ok {
			name = strings.Replace(name, "array", "list of "+d.typeOf(items), 1)
		}
	}
	if name == "" {
		return "any"
	}
	return name
}

func sortedKeys(o object) []string {
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func required(o object) []string {
	var out []string
	list, _ := o["required"].([]any)
	for _, r := range list {
		out = append(out, fmt.Sprint(r))
	}
	return out
}

// fields is a table of an object's fields.
func (d document) fields(b *bytes.Buffer, o object) {
	props, _ := o["properties"].(object)
	if len(props) == 0 {
		return
	}
	need := required(o)
	b.WriteString("| Field | Type | Required | About |\n|---|---|---|---|\n")
	for _, name := range sortedKeys(props) {
		p, _ := props[name].(object)
		yes := ""
		if slices.Contains(need, name) {
			yes = "yes"
		}
		about := str(p, "description")
		if about == "" {
			about = str(d.resolve(p), "description")
		}
		fmt.Fprintf(b, "| `%s` | %s | %s | %s |\n", name, oneLine(d.typeOf(p)), yes, oneLine(about))
	}
	b.WriteString("\n")
}

// parameters is a table of what goes in the path, the query and the headers: those the path
// declares for all its operations, then the operation's own, which win.
func (d document) parameters(b *bytes.Buffer, item, op object) {
	type parameter struct{ name, in string }
	found := map[parameter]object{}
	for _, source := range []object{item, op} {
		list, _ := source["parameters"].([]any)
		for _, v := range list {
			p := d.resolve(asObject(v))
			found[parameter{str(p, "name"), str(p, "in")}] = p
		}
	}
	if len(found) == 0 {
		return
	}
	keys := make([]parameter, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].in != keys[j].in {
			return keys[i].in < keys[j].in
		}
		return keys[i].name < keys[j].name
	})
	b.WriteString("**Parameters:**\n\n| Name | In | Type | Required | About |\n|---|---|---|---|---|\n")
	for _, k := range keys {
		p := found[k]
		yes := ""
		if required, _ := p["required"].(bool); required {
			yes = "yes"
		}
		fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s |\n", k.name, k.in, oneLine(d.typeOf(asObject(p["schema"]))), yes, oneLine(str(p, "description")))
	}
	b.WriteString("\n")
}

// variantName is what tells one of a oneOf's objects from another, if a const field does.
func variantName(o object) string {
	props, _ := o["properties"].(object)
	for _, key := range sortedKeys(props) {
		p, _ := props[key].(object)
		if c, ok := p["const"]; ok {
			return fmt.Sprintf("`%s: %v`", key, c)
		}
	}
	return ""
}

// body describes a request's JSON body.
func (d document) body(b *bytes.Buffer, op object) {
	rb := d.resolve(asObject(op["requestBody"]))
	content := asObject(rb["content"])
	if len(content) == 0 {
		return
	}
	about := oneLine(str(rb, "description"))
	for _, media := range sortedKeys(content) {
		schema := d.resolve(asObject(asObject(content[media])["schema"]))
		line := "**Request:** `" + media + "`."
		if about != "" {
			line += " " + about
		}
		b.WriteString(line + "\n\n")
		variants, isOneOf := schema["oneOf"].([]any)
		if !isOneOf {
			d.fields(b, schema)
			continue
		}
		for _, item := range variants {
			v := d.resolve(asObject(item))
			name := variantName(v)
			if name == "" {
				name = "One of"
			}
			var parts []string
			need := required(v)
			props := asObject(v["properties"])
			for _, field := range sortedKeys(props) {
				if strings.Contains(name, "`"+field+":") {
					continue
				}
				part := "`" + field + "`"
				if slices.Contains(need, field) {
					part += " (required)"
				}
				parts = append(parts, part)
			}
			if len(parts) == 0 {
				fmt.Fprintf(b, "- %s\n", name)
				continue
			}
			fmt.Fprintf(b, "- %s: %s\n", name, strings.Join(parts, ", "))
		}
		b.WriteString("\n")
	}
}

func asObject(v any) object {
	o, _ := v.(object)
	return o
}

func (d document) responses(b *bytes.Buffer, op object) {
	responses := asObject(op["responses"])
	if len(responses) == 0 {
		return
	}
	b.WriteString("**Responses:**\n\n")
	for _, code := range sortedKeys(responses) {
		r := d.resolve(asObject(responses[code]))
		line := "- `" + code + "`"
		if about := oneLine(str(r, "description")); about != "" {
			line += ": " + about
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
}

func permissionLine(op object) string {
	perm := str(op, "x-localbase-permission")
	if perm == "" {
		return ""
	}
	line := "**Needs:** `" + perm + "`"
	if v := op["x-localbase-or-permission"]; v != nil {
		line += fmt.Sprintf(" or `%v`", v)
	}
	if v := op["x-localbase-also-requires"]; v != nil {
		line += fmt.Sprintf(", and also `%v`", v)
	}
	return line + "."
}

// render is the whole page. It must depend on nothing but the document.
func render(raw []byte) ([]byte, error) {
	var root object
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	d := document{root}
	type operation struct {
		path, method string
		item, op     object
	}
	byTag := map[string][]operation{}
	paths := asObject(root["paths"])
	for _, p := range sortedKeys(paths) {
		item := asObject(paths[p])
		for _, method := range methods {
			op := asObject(item[method])
			if len(op) == 0 {
				continue
			}
			tag := "Other"
			if tags, ok := op["tags"].([]any); ok && len(tags) > 0 {
				tag = fmt.Sprint(tags[0])
			}
			byTag[tag] = append(byTag[tag], operation{p, method, item, op})
		}
	}

	var b bytes.Buffer
	b.WriteString("# API reference\n\n")
	fmt.Fprintf(&b, "<!-- Generated from go/%s. Do not edit this page: change the document, then run `%s` from go/. -->\n\n", documentPath, regenerate)
	if text := strings.TrimSpace(str(asObject(root["info"]), "description")); text != "" {
		b.WriteString(text + "\n\n")
	}
	b.WriteString("A running gateway serves this API's description at `GET /openapi.json`, and this page is generated from it.\n\n")
	b.WriteString("For how to use it, see the [API overview](api.md).\n\n")

	var order []string
	tags, _ := root["tags"].([]any)
	about := map[string]string{}
	for _, t := range tags {
		m := asObject(t)
		order = append(order, str(m, "name"))
		about[str(m, "name")] = str(m, "description")
	}
	for _, tag := range sortedKeysOf(byTag) {
		if !slices.Contains(order, tag) {
			order = append(order, tag)
		}
	}
	for _, tag := range order {
		ops := byTag[tag]
		if len(ops) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n", tag)
		if text := oneLine(about[tag]); text != "" {
			b.WriteString(text + "\n\n")
		}
		for _, o := range ops {
			fmt.Fprintf(&b, "### `%s %s`\n\n", strings.ToUpper(o.method), o.path)
			if text := oneLine(str(o.op, "summary")); text != "" {
				b.WriteString(text + ".\n\n")
			}
			if text := strings.TrimSpace(str(o.op, "description")); text != "" {
				b.WriteString(text + "\n\n")
			}
			if line := permissionLine(o.op); line != "" {
				b.WriteString(line + "\n\n")
			}
			d.parameters(&b, o.item, o.op)
			d.body(&b, o.op)
			d.responses(&b, o.op)
		}
	}
	return append(bytes.TrimRight(b.Bytes(), "\n"), '\n'), nil
}

func sortedKeysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func main() {
	check := flag.Bool("check", false, "Say whether the page is current, and change nothing")
	flag.Parse()
	raw, err := os.ReadFile(documentPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapi-docs: run this from go/:", err)
		os.Exit(1)
	}
	page, err := render(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapi-docs:", err)
		os.Exit(1)
	}
	if *check {
		current, err := os.ReadFile(pagePath)
		if err != nil || !bytes.Equal(current, page) {
			fmt.Fprintf(os.Stderr, "openapi-docs: %s is not current. Run: %s\n", pagePath, regenerate)
			os.Exit(1)
		}
		return
	}
	if err := os.WriteFile(pagePath, page, 0o644); err != nil { //nolint:gosec // G306: a page in the repository, readable like the rest of it.
		fmt.Fprintln(os.Stderr, "openapi-docs:", err)
		os.Exit(1)
	}
}
