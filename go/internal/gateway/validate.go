package gateway

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Request validation for the JSON inference routes. These port the TS Zod
// schemas: the same fields are checked with the same bounds, and failures
// produce `400 validation_failed` with "Validation failed: path: message".

type issue struct{ path, msg string }

type checker struct{ issues []issue }

func (c *checker) fail(path, msg string) { c.issues = append(c.issues, issue{path, msg}) }

func (c *checker) ok() bool { return len(c.issues) == 0 }

// reply writes the validation failure. The param is response_format when an
// issue is under it, as TS reports it.
func (c *checker) reply(w http.ResponseWriter, r *http.Request) {
	parts := make([]string, len(c.issues))
	var param *string
	for i, is := range c.issues {
		parts[i] = is.path + ": " + is.msg
		if is.path == "response_format" || strings.HasPrefix(is.path, "response_format.") {
			param = strp("response_format")
		}
	}
	badRequest(w, r, "Validation failed: "+strings.Join(parts, ", "), param)
}

func join(path string, key any) string {
	k := fmt.Sprint(key)
	if path == "" {
		return k
	}
	return path + "." + k
}

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case json.Number, float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

func expected(want string, v any, present bool) string {
	got := typeName(v)
	if !present {
		got = "undefined"
	}
	return "Invalid input: expected " + want + ", received " + got
}

// number reads a JSON number from a UseNumber decode.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	}
	return 0, false
}

// optNumber checks an optional number field against inclusive bounds; NaN
// bounds are open.
func (c *checker) optNumber(obj map[string]any, path, key string, lo, hi float64, loExclusive bool) {
	v, present := obj[key]
	if !present {
		return
	}
	f, ok := number(v)
	p := join(path, key)
	if !ok {
		c.fail(p, expected("number", v, true))
		return
	}
	if !math.IsNaN(lo) && (f < lo || (loExclusive && f == lo)) {
		if loExclusive {
			c.fail(p, "Too small: expected number to be >"+trimFloat(lo))
		} else {
			c.fail(p, "Too small: expected number to be >="+trimFloat(lo))
		}
	}
	if !math.IsNaN(hi) && f > hi {
		c.fail(p, "Too big: expected number to be <="+trimFloat(hi))
	}
}

func trimFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func (c *checker) optString(obj map[string]any, path, key string) {
	if v, present := obj[key]; present {
		if _, ok := v.(string); !ok {
			c.fail(join(path, key), expected("string", v, true))
		}
	}
}

func (c *checker) optBool(obj map[string]any, path, key string) {
	if v, present := obj[key]; present {
		if _, ok := v.(bool); !ok {
			c.fail(join(path, key), expected("boolean", v, true))
		}
	}
}

// strict reports keys outside allowed, as Zod's .strict() does.
func (c *checker) strict(obj map[string]any, path string, allowed ...string) {
	var extra []string
	for k := range obj {
		if !contains(allowed, k) {
			extra = append(extra, `"`+k+`"`)
		}
	}
	if len(extra) > 0 {
		sortStrings(extra)
		key := "key"
		if len(extra) > 1 {
			key = "keys"
		}
		c.fail(path, "Unrecognized "+key+": "+strings.Join(extra, ", "))
	}
}

func nonEmptyString(v any) bool {
	s, ok := v.(string)
	return ok && s != ""
}

// modelID checks the shared `model` rule: a string that is not blank.
func (c *checker) modelID(obj map[string]any) string {
	v, present := obj["model"]
	s, ok := v.(string)
	switch {
	case !ok:
		c.fail("model", expected("string", v, present))
	case strings.TrimSpace(s) == "":
		c.fail("model", "model must not be empty")
	}
	return s
}

// --- chat completions ----------------------------------------------------------

var schemaNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validateChat(req map[string]any) *checker {
	c := &checker{}
	c.modelID(req)
	if msgs, ok := req["messages"].([]any); !ok {
		v, present := req["messages"]
		c.fail("messages", expected("array", v, present))
	} else {
		for i, m := range msgs {
			c.chatMessage(m, join("messages", i))
		}
	}
	c.optNumber(req, "", "temperature", 0, 2, false)
	c.optNumber(req, "", "top_p", 0, 1, false)
	c.optNumber(req, "", "n", 1, math.NaN(), false)
	c.optBool(req, "", "stream")
	if v, present := req["stop"]; present && v != nil {
		switch s := v.(type) {
		case string:
		case []any:
			for i, e := range s {
				if _, ok := e.(string); !ok {
					c.fail(join("stop", i), expected("string", e, true))
				}
			}
		default:
			c.fail("stop", "Invalid input")
		}
	}
	c.optNumber(req, "", "max_tokens", 0, math.NaN(), true)
	c.optNumber(req, "", "max_completion_tokens", 0, math.NaN(), true)
	c.optNumber(req, "", "presence_penalty", -2, 2, false)
	c.optNumber(req, "", "frequency_penalty", -2, 2, false)
	if v, present := req["logit_bias"]; present && v != nil {
		if m, ok := v.(map[string]any); ok {
			for k, e := range m {
				if _, ok := number(e); !ok {
					c.fail(join("logit_bias", k), expected("number", e, true))
				}
			}
		} else {
			c.fail("logit_bias", expected("record", v, true))
		}
	}
	c.optString(req, "", "user")
	if v, present := req["response_format"]; present {
		c.responseFormat(v)
	}
	var toolNames []string
	if v, present := req["tools"]; present {
		if tools, ok := v.([]any); ok {
			for i, t := range tools {
				toolNames = append(toolNames, c.functionTool(t, join("tools", i)))
			}
		} else {
			c.fail("tools", expected("array", v, true))
		}
	}
	if v, present := req["tool_choice"]; present {
		switch tc := v.(type) {
		case string:
			if tc != "none" && tc != "auto" && tc != "required" {
				c.fail("tool_choice", "Invalid input")
			}
		case map[string]any:
			c.strict(tc, "tool_choice", "type", "function")
			if tc["type"] != "function" {
				c.fail("tool_choice.type", `Invalid input: expected "function"`)
			}
			fn, ok := tc["function"].(map[string]any)
			if !ok {
				c.fail("tool_choice.function", expected("object", tc["function"], true))
			} else {
				c.strict(fn, "tool_choice.function", "name")
				if !nonEmptyString(fn["name"]) {
					c.fail("tool_choice.function.name", "Too small: expected string to have >=1 characters")
				} else if c.ok() {
					matches := 0
					for _, n := range toolNames {
						if n == fn["name"] {
							matches++
						}
					}
					if matches != 1 {
						c.fail("tool_choice.function.name", "tool_choice.function.name must name exactly one declared function tool.")
					}
				}
			}
		default:
			c.fail("tool_choice", "Invalid input")
		}
	}
	return c
}

func (c *checker) chatMessage(m any, path string) {
	msg, ok := m.(map[string]any)
	if !ok {
		c.fail(path, expected("object", m, true))
		return
	}
	switch role := msg["role"]; role {
	case "system", "developer":
		switch content := msg["content"].(type) {
		case string:
		case []any:
			if len(content) == 0 {
				c.fail(join(path, "content"), "Too small: expected array to have >=1 items")
			}
			for i, p := range content {
				c.textPart(p, join(join(path, "content"), i))
			}
		default:
			c.fail(join(path, "content"), "Invalid input")
		}
		c.optString(msg, path, "name")
	case "user":
		switch content := msg["content"].(type) {
		case string:
		case []any:
			for i, p := range content {
				c.userPart(p, join(join(path, "content"), i))
			}
		default:
			c.fail(join(path, "content"), "Invalid input")
		}
		c.optString(msg, path, "name")
	case "assistant":
		if v, present := msg["content"]; present && v != nil {
			switch content := v.(type) {
			case string:
			case []any:
				for i, p := range content {
					pp := join(join(path, "content"), i)
					part, _ := p.(map[string]any)
					switch {
					case part != nil && part["type"] == "refusal":
						if _, ok := part["refusal"].(string); !ok {
							c.fail(join(pp, "refusal"), expected("string", part["refusal"], true))
						}
					default:
						c.textPart(p, pp)
					}
				}
			default:
				c.fail(join(path, "content"), "Invalid input")
			}
		}
		c.optString(msg, path, "name")
		calls := 0
		if v, present := msg["tool_calls"]; present {
			if list, ok := v.([]any); ok {
				calls = len(list)
				for i, tc := range list {
					c.toolCall(tc, join(join(path, "tool_calls"), i))
				}
			} else {
				c.fail(join(path, "tool_calls"), expected("array", v, true))
			}
		}
		for _, k := range []string{"refusal", "reasoning", "reasoning_content"} {
			if v, present := msg[k]; present && v != nil {
				if _, ok := v.(string); !ok {
					c.fail(join(path, k), expected("string", v, true))
				}
			}
		}
		if _, present := msg["function_call"]; present {
			c.fail(join(path, "function_call"), "Invalid input: expected never, received "+typeName(msg["function_call"]))
		}
		if msg["content"] == nil && calls == 0 && msg["refusal"] == nil && msg["reasoning"] == nil && msg["reasoning_content"] == nil {
			c.fail(path, "assistant messages require content, reasoning, tool_calls, or refusal")
		}
	case "tool":
		switch content := msg["content"].(type) {
		case string:
		case []any:
			for i, p := range content {
				c.textPart(p, join(join(path, "content"), i))
			}
		default:
			c.fail(join(path, "content"), "Invalid input")
		}
		if !nonEmptyString(msg["tool_call_id"]) {
			c.fail(join(path, "tool_call_id"), "Too small: expected string to have >=1 characters")
		}
	default:
		c.fail(join(path, "role"), `Invalid input: expected "system" | "developer" | "user" | "assistant" | "tool"`)
	}
}

func (c *checker) textPart(p any, path string) {
	part, ok := p.(map[string]any)
	if !ok {
		c.fail(path, expected("object", p, true))
		return
	}
	if part["type"] != "text" {
		c.fail(join(path, "type"), `Invalid input: expected "text"`)
	}
	if _, ok := part["text"].(string); !ok {
		c.fail(join(path, "text"), expected("string", part["text"], true))
	}
}

func (c *checker) userPart(p any, path string) {
	part, ok := p.(map[string]any)
	if !ok {
		c.fail(path, "Invalid input")
		return
	}
	switch part["type"] {
	case "text":
		c.textPart(p, path)
	case "image_url":
		img, ok := part["image_url"].(map[string]any)
		if !ok || !nonEmptyString(img["url"]) {
			c.fail(join(path, "image_url"), "Invalid input")
			return
		}
		if d, present := img["detail"]; present && d != "auto" && d != "low" && d != "high" {
			c.fail(join(path, "image_url.detail"), `Invalid option: expected one of "auto"|"low"|"high"`)
		}
	case "input_audio":
		a, ok := part["input_audio"].(map[string]any)
		if !ok || !nonEmptyString(a["data"]) || (a["format"] != "wav" && a["format"] != "mp3") {
			c.fail(join(path, "input_audio"), "Invalid input")
		}
	case "file":
		f, ok := part["file"].(map[string]any)
		valid := ok && ((len(f) == 1 && nonEmptyString(f["file_id"])) ||
			(len(f) == 2 && nonEmptyString(f["filename"]) && nonEmptyString(f["file_data"])))
		if !valid {
			c.fail(join(path, "file"), "Invalid input")
		}
	default:
		c.fail(path, "Invalid input")
	}
}

func (c *checker) toolCall(tc any, path string) {
	call, ok := tc.(map[string]any)
	if !ok {
		c.fail(path, expected("object", tc, true))
		return
	}
	if !nonEmptyString(call["id"]) {
		c.fail(join(path, "id"), "Too small: expected string to have >=1 characters")
	}
	if call["type"] != "function" {
		c.fail(join(path, "type"), `Invalid input: expected "function"`)
	}
	fn, ok := call["function"].(map[string]any)
	if !ok {
		c.fail(join(path, "function"), expected("object", call["function"], true))
		return
	}
	if !nonEmptyString(fn["name"]) {
		c.fail(join(path, "function.name"), "Too small: expected string to have >=1 characters")
	}
	if _, ok := fn["arguments"].(string); !ok {
		c.fail(join(path, "function.arguments"), expected("string", fn["arguments"], true))
	}
}

// functionTool validates one tools[] entry and returns its name.
func (c *checker) functionTool(t any, path string) string {
	tool, ok := t.(map[string]any)
	if !ok {
		c.fail(path, expected("object", t, true))
		return ""
	}
	c.strict(tool, path, "type", "function")
	if tool["type"] != "function" {
		c.fail(join(path, "type"), `Invalid input: expected "function"`)
	}
	fn, ok := tool["function"].(map[string]any)
	if !ok {
		c.fail(join(path, "function"), expected("object", tool["function"], true))
		return ""
	}
	fp := join(path, "function")
	c.strict(fn, fp, "name", "description", "parameters", "strict")
	name, _ := fn["name"].(string)
	if name == "" {
		c.fail(join(fp, "name"), "Too small: expected string to have >=1 characters")
	}
	c.optString(fn, fp, "description")
	if v, present := fn["parameters"]; present {
		if _, ok := v.(map[string]any); !ok {
			c.fail(join(fp, "parameters"), expected("record", v, true))
		}
	}
	c.optBool(fn, fp, "strict")
	return name
}

func (c *checker) responseFormat(v any) {
	rf, ok := v.(map[string]any)
	if !ok {
		c.fail("response_format", expected("object", v, true))
		return
	}
	switch rf["type"] {
	case "text", "json_object":
		c.strict(rf, "response_format", "type")
	case "json_schema":
		c.strict(rf, "response_format", "type", "json_schema")
		js, ok := rf["json_schema"].(map[string]any)
		if !ok {
			c.fail("response_format.json_schema", expected("object", rf["json_schema"], true))
			return
		}
		c.strict(js, "response_format.json_schema", "name", "description", "schema", "strict")
		name, _ := js["name"].(string)
		switch {
		case name == "":
			c.fail("response_format.json_schema.name", "Too small: expected string to have >=1 characters")
		case len([]rune(name)) > 64:
			c.fail("response_format.json_schema.name", "Too big: expected string to have <=64 characters")
		case !schemaNameRe.MatchString(name):
			c.fail("response_format.json_schema.name", "Invalid string: must match pattern /^[A-Za-z0-9_-]+$/")
		}
		c.optString(js, "response_format.json_schema", "description")
		if _, ok := js["schema"].(map[string]any); !ok {
			c.fail("response_format.json_schema.schema", expected("record", js["schema"], true))
		}
		c.optBool(js, "response_format.json_schema", "strict")
	default:
		c.fail("response_format.type", `Invalid input: expected "text" | "json_object" | "json_schema"`)
	}
}

// --- embeddings and systemone --------------------------------------------------

func validateEmbeddings(req map[string]any) *checker {
	c := &checker{}
	c.modelID(req)
	switch in := req["input"].(type) {
	case string:
	case []any:
		allStr, allNum, allArr := true, true, true
		for _, e := range in {
			_, s := e.(string)
			_, n := number(e)
			arr, a := e.([]any)
			if a {
				for _, x := range arr {
					if _, ok := number(x); !ok {
						a = false
					}
				}
			}
			allStr, allNum, allArr = allStr && s, allNum && n, allArr && a
		}
		if !allStr && !allNum && !allArr {
			c.fail("input", "Invalid input")
		}
	default:
		c.fail("input", "Invalid input")
	}
	if v, present := req["encoding_format"]; present && v != "float" {
		c.fail("encoding_format", `Invalid input: expected "float"`)
	}
	if v, present := req["dimensions"]; present {
		f, ok := number(v)
		if !ok || f != math.Trunc(f) || f <= 0 {
			c.fail("dimensions", "Invalid input: expected a positive integer")
		}
	}
	c.optString(req, "", "user")
	return c
}

var decisionFileRe = regexp.MustCompile(`(?i)^data:(image|audio)/[a-z0-9.+-]+;base64,`)

const maxDecisionChoiceOptions = 255

func validateDecision(req map[string]any) *checker {
	c := &checker{}
	c.strict(req, "", "model", "state", "files", "images", "questions")
	c.modelID(req)
	// TS declares state required but its schema accepts a missing one.
	if v, present := req["state"]; present {
		switch v.(type) {
		case nil, string, map[string]any, []any:
		default:
			c.fail("state", "Invalid input")
		}
	}
	for _, key := range []string{"files", "images"} {
		v, present := req[key]
		if !present {
			continue
		}
		list, ok := v.([]any)
		if !ok {
			c.fail(key, expected("array", v, true))
			continue
		}
		for i, f := range list {
			s, _ := f.(string)
			if !decisionFileRe.MatchString(s) {
				c.fail(join(key, i), "Each file must be a base64 data URL (data:image/... or data:audio/...).")
			}
		}
	}
	qs, ok := req["questions"].(map[string]any)
	if !ok {
		v, present := req["questions"]
		c.fail("questions", expected("record", v, present))
		return c
	}
	if len(qs) == 0 {
		c.fail("questions", "questions must contain at least one question.")
	}
	for _, id := range sortedKeys(qs) {
		path := join("questions", id)
		q, ok := qs[id].(map[string]any)
		if !ok {
			c.fail(path, expected("object", qs[id], true))
			continue
		}
		if v, present := q["instructions"]; present {
			switch v.(type) {
			case string, map[string]any, []any:
			default:
				c.fail(join(path, "instructions"), "Invalid input")
			}
		}
		switch q["type"] {
		case "choice":
			c.strict(q, path, "type", "instructions", "criteria")
			crit, ok := q["criteria"].(map[string]any)
			if !ok {
				c.fail(join(path, "criteria"), expected("record", q["criteria"], true))
				break
			}
			for k, v := range crit {
				if k == "" {
					c.fail(join(path, "criteria"), "Too small: expected string to have >=1 characters")
				}
				if _, ok := v.(string); !ok && v != nil {
					c.fail(join(join(path, "criteria"), k), expected("string", v, true))
				}
			}
			if len(crit) < 2 {
				c.fail(join(path, "criteria"), "A choice question needs at least 2 options.")
			}
			if len(crit) > maxDecisionChoiceOptions {
				c.fail(join(path, "criteria"), fmt.Sprintf("A choice question accepts at most %d options.", maxDecisionChoiceOptions))
			}
		case "score":
			c.strict(q, path, "type", "instructions", "criteria")
			crit, ok := q["criteria"].([]any)
			if !ok {
				c.fail(join(path, "criteria"), expected("array", q["criteria"], true))
				break
			}
			for i, v := range crit {
				if _, ok := v.(string); !ok {
					c.fail(join(join(path, "criteria"), i), expected("string", v, true))
				}
			}
			if len(crit) < 2 {
				c.fail(join(path, "criteria"), "Too small: expected array to have >=2 items")
			}
			if len(crit) > 10 {
				c.fail(join(path, "criteria"), "Too big: expected array to have <=10 items")
			}
		case "noul":
			c.strict(q, path, "type", "instructions", "criteria")
			if v, present := q["criteria"]; present {
				crit, ok := v.(map[string]any)
				if !ok {
					c.fail(join(path, "criteria"), expected("object", v, true))
					break
				}
				c.strict(crit, join(path, "criteria"), "true", "false")
				c.optString(crit, join(path, "criteria"), "true")
				c.optString(crit, join(path, "criteria"), "false")
			}
		default:
			c.fail(join(path, "type"), `Invalid input: expected "choice" | "score" | "noul"`)
		}
	}
	return c
}
