// Package toml reads and writes the TOML subset LocalBase configuration
// uses: tables and arrays of tables, dotted keys, basic and literal strings
// (single and multi-line), integers, floats, booleans, arrays and inline
// tables. Dates are not supported. It replaces a third-party dependency.
package toml

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Parse decodes a document into nested map[string]any values: string,
// int64, float64, bool, []any and map[string]any.
func Parse(src string) (map[string]any, error) {
	p := &parser{src: src, line: 1}
	root := map[string]any{}
	p.root, p.cur = root, root
	if err := p.document(); err != nil {
		return nil, err
	}
	return root, nil
}

type parser struct {
	src     string
	pos     int
	line    int
	root    map[string]any
	cur     map[string]any
	defined map[string]bool // explicitly defined tables
}

func (p *parser) errf(format string, a ...any) error {
	return fmt.Errorf("line %d: %s", p.line, fmt.Sprintf(format, a...))
}

func (p *parser) eof() bool { return p.pos >= len(p.src) }

func (p *parser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.src[p.pos]
}

func (p *parser) skipSpace() {
	for !p.eof() && (p.peek() == ' ' || p.peek() == '\t') {
		p.pos++
	}
}

// skipBlank skips whitespace, newlines and comments.
func (p *parser) skipBlank() {
	for !p.eof() {
		switch c := p.peek(); {
		case c == ' ' || c == '\t' || c == '\r':
			p.pos++
		case c == '\n':
			p.pos++
			p.line++
		case c == '#':
			for !p.eof() && p.peek() != '\n' {
				p.pos++
			}
		default:
			return
		}
	}
}

func (p *parser) endOfLine() error {
	p.skipSpace()
	if p.peek() == '#' {
		for !p.eof() && p.peek() != '\n' {
			p.pos++
		}
	}
	if p.peek() == '\r' {
		p.pos++
	}
	if p.eof() {
		return nil
	}
	if p.peek() != '\n' {
		return p.errf("expected the end of the line")
	}
	return nil
}

func (p *parser) document() error {
	p.defined = map[string]bool{}
	for {
		p.skipBlank()
		if p.eof() {
			return nil
		}
		if p.peek() == '[' {
			if err := p.tableHeader(); err != nil {
				return err
			}
		} else if err := p.keyValue(p.cur); err != nil {
			return err
		}
		if err := p.endOfLine(); err != nil {
			return err
		}
	}
}

func (p *parser) tableHeader() error {
	p.pos++
	array := p.peek() == '['
	if array {
		p.pos++
	}
	p.skipSpace()
	keys, err := p.key()
	if err != nil {
		return err
	}
	p.skipSpace()
	if p.peek() != ']' {
		return p.errf("expected ]")
	}
	p.pos++
	if array {
		if p.peek() != ']' {
			return p.errf("expected ]]")
		}
		p.pos++
	}
	parent := p.root
	for _, k := range keys[:len(keys)-1] {
		next, err := descend(parent, k)
		if err != nil {
			return p.errf("%v", err)
		}
		parent = next
	}
	last := keys[len(keys)-1]
	if array {
		existing, ok := parent[last]
		if !ok {
			existing = []any{}
		}
		list, ok := existing.([]any)
		if !ok {
			return p.errf("%s is not an array of tables", last)
		}
		t := map[string]any{}
		parent[last] = append(list, t)
		p.cur = t
		return nil
	}
	name := strings.Join(keys, ".")
	if p.defined[name] {
		return p.errf("table %s is defined twice", name)
	}
	p.defined[name] = true
	switch v := parent[last].(type) {
	case nil:
		t := map[string]any{}
		parent[last] = t
		p.cur = t
	case map[string]any:
		p.cur = v
	default:
		return p.errf("%s is already a value", name)
	}
	return nil
}

// descend returns the table at key k, creating it, or the last table of an
// array of tables.
func descend(m map[string]any, k string) (map[string]any, error) {
	switch v := m[k].(type) {
	case nil:
		t := map[string]any{}
		m[k] = t
		return t, nil
	case map[string]any:
		return v, nil
	case []any:
		if len(v) > 0 {
			if t, ok := v[len(v)-1].(map[string]any); ok {
				return t, nil
			}
		}
	}
	return nil, fmt.Errorf("%s is already a value", k)
}

func isBare(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func (p *parser) key() ([]string, error) {
	var keys []string
	for {
		p.skipSpace()
		var k string
		switch c := p.peek(); {
		case c == '"':
			s, err := p.basicString()
			if err != nil {
				return nil, err
			}
			k = s
		case c == '\'':
			s, err := p.literalString()
			if err != nil {
				return nil, err
			}
			k = s
		case isBare(c):
			start := p.pos
			for !p.eof() && isBare(p.peek()) {
				p.pos++
			}
			k = p.src[start:p.pos]
		default:
			return nil, p.errf("expected a key")
		}
		keys = append(keys, k)
		p.skipSpace()
		if p.peek() != '.' {
			return keys, nil
		}
		p.pos++
	}
}

func (p *parser) keyValue(into map[string]any) error {
	keys, err := p.key()
	if err != nil {
		return err
	}
	p.skipSpace()
	if p.peek() != '=' {
		return p.errf("expected =")
	}
	p.pos++
	p.skipSpace()
	v, err := p.value()
	if err != nil {
		return err
	}
	t := into
	for _, k := range keys[:len(keys)-1] {
		if t, err = descend(t, k); err != nil {
			return p.errf("%v", err)
		}
	}
	last := keys[len(keys)-1]
	if _, dup := t[last]; dup {
		return p.errf("duplicate key %s", strings.Join(keys, "."))
	}
	t[last] = v
	return nil
}

func (p *parser) value() (any, error) {
	switch c := p.peek(); {
	case c == '"':
		if strings.HasPrefix(p.src[p.pos:], `"""`) {
			return p.multilineBasic()
		}
		return p.basicString()
	case c == '\'':
		if strings.HasPrefix(p.src[p.pos:], `'''`) {
			return p.multilineLiteral()
		}
		return p.literalString()
	case c == '[':
		return p.array()
	case c == '{':
		return p.inlineTable()
	case c == 't' && strings.HasPrefix(p.src[p.pos:], "true"):
		p.pos += 4
		return true, nil
	case c == 'f' && strings.HasPrefix(p.src[p.pos:], "false"):
		p.pos += 5
		return false, nil
	}
	return p.number()
}

func (p *parser) number() (any, error) {
	start := p.pos
	for !p.eof() && strings.IndexByte("+-0123456789._eExabcdfinABCDF", p.peek()) >= 0 {
		p.pos++
	}
	raw := p.src[start:p.pos]
	if raw == "" {
		return nil, p.errf("expected a value")
	}
	clean := strings.ReplaceAll(raw, "_", "")
	switch strings.TrimLeft(clean, "+-") {
	case "inf":
		if strings.HasPrefix(clean, "-") {
			return math.Inf(-1), nil
		}
		return math.Inf(1), nil
	case "nan":
		return math.NaN(), nil
	}
	if strings.HasPrefix(clean, "0x") || strings.HasPrefix(clean, "0o") || strings.HasPrefix(clean, "0b") {
		base := map[byte]int{'x': 16, 'o': 8, 'b': 2}[clean[1]]
		n, err := strconv.ParseInt(clean[2:], base, 64)
		if err != nil {
			return nil, p.errf("invalid number %s", raw)
		}
		return n, nil
	}
	if !strings.ContainsAny(clean, ".eE") {
		n, err := strconv.ParseInt(clean, 10, 64)
		if err != nil {
			return nil, p.errf("invalid number %s", raw)
		}
		return n, nil
	}
	f, err := strconv.ParseFloat(clean, 64)
	if err != nil {
		return nil, p.errf("invalid number %s", raw)
	}
	return f, nil
}

func (p *parser) escape(b *strings.Builder) error {
	p.pos++ // backslash
	if p.eof() {
		return p.errf("unterminated escape")
	}
	c := p.peek()
	p.pos++
	switch c {
	case 'b':
		b.WriteByte('\b')
	case 't':
		b.WriteByte('\t')
	case 'n':
		b.WriteByte('\n')
	case 'f':
		b.WriteByte('\f')
	case 'r':
		b.WriteByte('\r')
	case 'e':
		b.WriteByte(0x1b)
	case '"':
		b.WriteByte('"')
	case '\\':
		b.WriteByte('\\')
	case 'u', 'U':
		n := 4
		if c == 'U' {
			n = 8
		}
		if p.pos+n > len(p.src) {
			return p.errf("invalid unicode escape")
		}
		r, err := strconv.ParseUint(p.src[p.pos:p.pos+n], 16, 32)
		if err != nil || !utf8.ValidRune(rune(r)) {
			return p.errf("invalid unicode escape")
		}
		b.WriteRune(rune(r))
		p.pos += n
	default:
		return p.errf("invalid escape \\%c", c)
	}
	return nil
}

func (p *parser) basicString() (string, error) {
	p.pos++
	var b strings.Builder
	for {
		if p.eof() || p.peek() == '\n' {
			return "", p.errf("unterminated string")
		}
		switch c := p.peek(); c {
		case '"':
			p.pos++
			return b.String(), nil
		case '\\':
			if err := p.escape(&b); err != nil {
				return "", err
			}
		default:
			b.WriteByte(c)
			p.pos++
		}
	}
}

func (p *parser) literalString() (string, error) {
	p.pos++
	end := strings.IndexAny(p.src[p.pos:], "'\n")
	if end < 0 || p.src[p.pos+end] != '\'' {
		return "", p.errf("unterminated string")
	}
	s := p.src[p.pos : p.pos+end]
	p.pos += end + 1
	return s, nil
}

func (p *parser) multilineBasic() (string, error) {
	p.pos += 3
	if strings.HasPrefix(p.src[p.pos:], "\r\n") {
		p.pos += 2
		p.line++
	} else if p.peek() == '\n' {
		p.pos++
		p.line++
	}
	var b strings.Builder
	for {
		if p.eof() {
			return "", p.errf("unterminated string")
		}
		if strings.HasPrefix(p.src[p.pos:], `"""`) {
			p.pos += 3
			for p.peek() == '"' { // up to two quotes may close the content
				b.WriteByte('"')
				p.pos++
			}
			return b.String(), nil
		}
		c := p.peek()
		if c == '\\' {
			// A line-ending backslash trims the newline and leading space.
			rest := p.src[p.pos+1:]
			trimmed := strings.TrimLeft(rest, " \t")
			if strings.HasPrefix(trimmed, "\n") || strings.HasPrefix(trimmed, "\r\n") {
				p.pos++
				for !p.eof() && strings.IndexByte(" \t\r\n", p.peek()) >= 0 {
					if p.peek() == '\n' {
						p.line++
					}
					p.pos++
				}
				continue
			}
			if err := p.escape(&b); err != nil {
				return "", err
			}
			continue
		}
		if c == '\n' {
			p.line++
		}
		b.WriteByte(c)
		p.pos++
	}
}

func (p *parser) multilineLiteral() (string, error) {
	p.pos += 3
	if strings.HasPrefix(p.src[p.pos:], "\r\n") {
		p.pos += 2
		p.line++
	} else if p.peek() == '\n' {
		p.pos++
		p.line++
	}
	end := strings.Index(p.src[p.pos:], "'''")
	if end < 0 {
		return "", p.errf("unterminated string")
	}
	for strings.HasPrefix(p.src[p.pos+end+1:], "'''") && end+1 <= len(p.src)-p.pos-3 {
		end++
	}
	s := p.src[p.pos : p.pos+end]
	p.line += strings.Count(s, "\n")
	p.pos += end + 3
	return s, nil
}

func (p *parser) array() ([]any, error) {
	p.pos++
	out := []any{}
	for {
		p.skipBlank()
		if p.peek() == ']' {
			p.pos++
			return out, nil
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.skipBlank()
		switch p.peek() {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return out, nil
		default:
			return nil, p.errf("expected , or ] in an array")
		}
	}
}

func (p *parser) inlineTable() (map[string]any, error) {
	p.pos++
	out := map[string]any{}
	p.skipSpace()
	if p.peek() == '}' {
		p.pos++
		return out, nil
	}
	for {
		p.skipSpace()
		if err := p.keyValue(out); err != nil {
			return nil, err
		}
		p.skipSpace()
		switch p.peek() {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return out, nil
		default:
			return nil, p.errf("expected , or } in an inline table")
		}
	}
}

// --- writing ----------------------------------------------------------------

// Quote encodes a basic string.
func Quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Value encodes a scalar or an array of scalars.
func Value(v any) string {
	switch x := v.(type) {
	case string:
		return Quote(x)
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatFloat(x, 'f', 1, 64)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case []string:
		parts := make([]string, len(x))
		for i, s := range x {
			parts[i] = Quote(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = Value(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return Quote(fmt.Sprint(v))
}

// SortedKeys returns a map's keys in order.
func SortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
