package jsjson

import (
	"strings"
	"testing"
)

func nested(open, shut string, depth int) string {
	return strings.Repeat(open, depth) + strings.Repeat(shut, depth)
}

func TestNestingUpToTheLimitParses(t *testing.T) {
	for name, doc := range map[string]string{
		"arrays":  nested("[", "]", maxParseDepth),
		"objects": strings.Repeat(`{"a":`, maxParseDepth-1) + "{}" + strings.Repeat("}", maxParseDepth-1),
		"mixed":   strings.Repeat(`[{"a":`, maxParseDepth/2-1) + "[{}]" + strings.Repeat("}]", maxParseDepth/2-1),
	} {
		v, err := Parse([]byte(doc))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := string(Stringify(v)); got != doc {
			t.Errorf("%s didn't come back the same", name)
		}
	}
}

func TestNestingPastTheLimitIsRefused(t *testing.T) {
	for name, doc := range map[string]string{
		"arrays":  nested("[", "]", maxParseDepth+1),
		"objects": strings.Repeat(`{"a":`, maxParseDepth) + "{}" + strings.Repeat("}", maxParseDepth),
		"mixed":   strings.Repeat(`[{"a":`, maxParseDepth/2) + "[]" + strings.Repeat("}]", maxParseDepth/2),
	} {
		if v, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted: %T", name, v)
		}
	}
}

// As much as a feed may be, all of it nesting, and never closed. Without a limit this overflows
// the stack, which ends the process: recover can't catch it.
func TestADocumentOfNothingButNestingIsRefused(t *testing.T) {
	for name, doc := range map[string]string{
		"arrays":  strings.Repeat("[", 8<<20),
		"objects": strings.Repeat(`{"a":`, (8<<20)/5),
	} {
		if v, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted: %T", name, v)
		}
	}
}

// Wide isn't deep: siblings don't add up.
func TestManySiblingsAreNotNesting(t *testing.T) {
	doc := "[" + strings.Repeat("[[]],", maxParseDepth*2) + "[]]"
	if _, err := Parse([]byte(doc)); err != nil {
		t.Error(err)
	}
}

func TestTheDepthLimitIsTheStatedOne(t *testing.T) {
	if maxParseDepth != 1000 {
		t.Errorf("%d", maxParseDepth)
	}
}

// Whatever a parse accepts comes back the same when it's written and read again.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		`null`, `true`, `1e400`, `-0`, `"a\u0000b"`, `[]`, `{}`, `{"b":1,"a":2,"1":3}`, `[1,[2,[3,{"a":[]}]]]`,
		`{"a":{"a":{"a":1}}}`, `[`, `{"a":`, `]`, `{"a"}`, `[1,]`, `"\ud800"`,
		nested("[", "]", maxParseDepth), nested("[", "]", maxParseDepth+1), strings.Repeat("[", 4096),
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := Parse(data)
		if err != nil {
			return
		}
		once := Stringify(v)
		w, err := Parse(once)
		if err != nil {
			t.Fatalf("%q came back as %q, which doesn't parse: %v", data, once, err)
		}
		if twice := Stringify(w); string(twice) != string(once) {
			t.Fatalf("%q came back as %q, then %q", data, once, twice)
		}
	})
}
