package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
)

// MergeMisaki builds CrispASR's Kokoro G2P dictionary from the pinned misaki
// us_gold.json and us_silver.json: every gold entry, then silver entries gold
// lacks, written as compact JSON with a trailing newline. It reproduces the
// file the 2026-10-20 TTS screen used byte for byte (JavaScript object
// semantics: { ...gold } plus missing silver keys, JSON.stringify).
func MergeMisaki(goldPath, silverPath string) ([]byte, error) {
	read := func(p string) (*jsObject, error) {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		v, err := parseJS(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		o, ok := v.(*jsObject)
		if !ok {
			return nil, errors.New("dictionary is not a JSON object")
		}
		return o, nil
	}
	gold, err := read(goldPath)
	if err != nil {
		return nil, err
	}
	silver, err := read(silverPath)
	if err != nil {
		return nil, err
	}
	merged := &jsObject{index: map[string]int{}}
	for _, k := range gold.keys() {
		merged.set(k, gold.vals[gold.index[k]])
	}
	for _, k := range silver.keys() {
		if _, ok := merged.index[k]; !ok {
			merged.set(k, silver.vals[silver.index[k]])
		}
	}
	var out bytes.Buffer
	stringifyJS(&out, merged)
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// jsObject is a JSON object with JavaScript's property order: array-index
// keys ascending, then other keys in insertion order (a repeated key keeps
// its first position and its last value).
type jsObject struct {
	order []string
	vals  []any
	index map[string]int
}

func (o *jsObject) set(k string, v any) {
	if i, ok := o.index[k]; ok {
		o.vals[i] = v
		return
	}
	o.index[k] = len(o.order)
	o.order = append(o.order, k)
	o.vals = append(o.vals, v)
}

func isArrayIndex(k string) (uint64, bool) {
	if k == "" || (len(k) > 1 && k[0] == '0') {
		return 0, false
	}
	n, err := strconv.ParseUint(k, 10, 64)
	return n, err == nil && n < 1<<32-1
}

func (o *jsObject) keys() []string {
	var idx, rest []string
	for _, k := range o.order {
		if _, ok := isArrayIndex(k); ok {
			idx = append(idx, k)
		} else {
			rest = append(rest, k)
		}
	}
	sort.Slice(idx, func(i, j int) bool {
		a, _ := isArrayIndex(idx[i])
		b, _ := isArrayIndex(idx[j])
		return a < b
	})
	return append(idx, rest...)
}

// parseJS decodes JSON keeping object order; numbers stay json.Number.
func parseJS(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, errors.New("trailing data")
	}
	return v, nil
}

func parseValue(dec *json.Decoder) (any, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := t.(type) {
	case json.Delim:
		switch v {
		case '{':
			o := &jsObject{index: map[string]int{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				val, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				o.set(kt.(string), val)
			}
			_, err := dec.Token()
			return o, err
		case '[':
			var arr []any
			for dec.More() {
				val, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			_, err := dec.Token()
			if arr == nil {
				arr = []any{}
			}
			return arr, err
		}
	}
	return t, nil
}

// stringifyJS writes JSON.stringify(v) (no indentation).
func stringifyJS(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case *jsObject:
		b.WriteByte('{')
		for i, k := range x.keys() {
			if i > 0 {
				b.WriteByte(',')
			}
			quoteJS(b, k)
			b.WriteByte(':')
			stringifyJS(b, x.vals[x.index[k]])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			stringifyJS(b, e)
		}
		b.WriteByte(']')
	case string:
		quoteJS(b, x)
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			b.WriteString(x.String())
			return
		}
		b.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case nil:
		b.WriteString("null")
	}
}

// quoteJS escapes like JSON.stringify: quote, backslash and control
// characters only; everything else is written as UTF-8.
func quoteJS(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(fmt.Sprintf(`\u%04x`, r))
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}
