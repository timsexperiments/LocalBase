// Package jsjson reproduces JavaScript's JSON round trip: JSON.parse keeps
// object property order (array-index keys first, ascending, then insertion
// order) and JSON.stringify writes numbers and strings the way V8 and
// JavaScriptCore do. Checksums the TS code computes over JSON.stringify
// output (catalog feeds, the Misaki dictionary) are reproducible with it.
package jsjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Object is a JSON object with JavaScript's property order. A repeated key
// keeps its first position and its last value.
type Object struct {
	order []string
	vals  []any
	index map[string]int
}

func NewObject() *Object { return &Object{index: map[string]int{}} }

// Set adds or replaces a property.
func (o *Object) Set(k string, v any) {
	if i, ok := o.index[k]; ok {
		o.vals[i] = v
		return
	}
	o.index[k] = len(o.order)
	o.order = append(o.order, k)
	o.vals = append(o.vals, v)
}

// Get returns a property.
func (o *Object) Get(k string) (any, bool) {
	i, ok := o.index[k]
	if !ok {
		return nil, false
	}
	return o.vals[i], true
}

// Len is the number of properties.
func (o *Object) Len() int { return len(o.order) }

func arrayIndex(k string) (uint64, bool) {
	if k == "" || (len(k) > 1 && k[0] == '0') {
		return 0, false
	}
	n, err := strconv.ParseUint(k, 10, 64)
	return n, err == nil && n < 1<<32-1
}

// Keys lists the properties in JavaScript enumeration order.
func (o *Object) Keys() []string {
	var idx, rest []string
	for _, k := range o.order {
		if _, ok := arrayIndex(k); ok {
			idx = append(idx, k)
		} else {
			rest = append(rest, k)
		}
	}
	sort.Slice(idx, func(i, j int) bool {
		a, _ := arrayIndex(idx[i])
		b, _ := arrayIndex(idx[j])
		return a < b
	})
	return append(idx, rest...)
}

// Parse decodes JSON like JSON.parse. Objects are *Object, arrays []any,
// numbers float64, plus string, bool and nil.
func Parse(b []byte) (any, error) {
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
			o := NewObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				val, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				o.Set(kt.(string), val)
			}
			_, err := dec.Token()
			return o, err
		case '[':
			arr := []any{}
			for dec.More() {
				val, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			_, err := dec.Token()
			return arr, err
		}
	case json.Number:
		// JSON.parse yields a double; overflow is Infinity.
		f, err := strconv.ParseFloat(v.String(), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, err
		}
		return f, nil
	}
	return t, nil
}

// Stringify writes JSON.stringify(v) without indentation.
func Stringify(v any) []byte {
	var b bytes.Buffer
	write(&b, v)
	return b.Bytes()
}

func write(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case *Object:
		b.WriteByte('{')
		for i, k := range x.Keys() {
			if i > 0 {
				b.WriteByte(',')
			}
			quote(b, k)
			b.WriteByte(':')
			write(b, x.vals[x.index[k]])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			write(b, e)
		}
		b.WriteByte(']')
	case string:
		quote(b, x)
	case float64:
		b.WriteString(Number(x))
	case json.Number:
		f, _ := x.Float64()
		b.WriteString(Number(f))
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case nil:
		b.WriteString("null")
	}
}

// Number formats a double like JavaScript's Number.prototype.toString.
func Number(f float64) string {
	switch {
	case math.IsNaN(f) || math.IsInf(f, 0):
		return "null" // JSON.stringify
	case f == 0:
		return "0"
	}
	abs := math.Abs(f)
	if abs >= 1e21 || abs < 1e-6 {
		s := strconv.FormatFloat(f, 'e', -1, 64) // 1e+21, 1.5e-7
		mant, exp, _ := strings.Cut(s, "e")
		sign := exp[0]
		exp = strings.TrimLeft(exp[1:], "0")
		return mant + "e" + string(sign) + exp
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// quote escapes like JSON.stringify: quote, backslash, control characters
// and lone surrogates; everything else as UTF-8.
func quote(b *bytes.Buffer, s string) {
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
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// Indent writes JSON.stringify(v, null, 2).
func Indent(v any) []byte {
	var b bytes.Buffer
	writeIndent(&b, v, "")
	return b.Bytes()
}

func writeIndent(b *bytes.Buffer, v any, pad string) {
	inner := pad + "  "
	switch x := v.(type) {
	case *Object:
		keys := x.Keys()
		if len(keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range keys {
			b.WriteString(inner)
			quote(b, k)
			b.WriteString(": ")
			writeIndent(b, x.vals[x.index[k]], inner)
			if i < len(keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(pad + "}")
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, e := range x {
			b.WriteString(inner)
			writeIndent(b, e, inner)
			if i < len(x)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(pad + "]")
	default:
		write(b, v)
	}
}

// FromGo converts an encoding/json-encodable value (structs keep their
// field order, maps their sorted keys) into jsjson values.
func FromGo(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	out, _ := Parse(b)
	return out
}

// ToFixed1 is Number.prototype.toFixed(1) for non-negative finite values:
// the exact decimal value, ties toward the larger number.
func ToFixed1(x float64) string {
	digits := strconv.FormatFloat(x, 'f', 80, 64) // exact above 2^-28
	dot := strings.IndexByte(digits, '.')
	n, _ := strconv.Atoi(digits[:dot] + digits[dot+1:dot+2])
	if digits[dot+2] >= '5' {
		n++
	}
	return fmt.Sprintf("%d.%d", n/10, n%10)
}
