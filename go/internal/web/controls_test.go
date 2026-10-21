package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestControlsAtLeast16px keeps iOS Safari from zooming when a field gets
// focus: every visible input, textarea and select renders at 16px or more.
// The .input class is text-base (16px); a control without it must say
// text-base, and none may shrink itself with a smaller text size.
func TestControlsAtLeast16px(t *testing.T) {
	files, _ := filepath.Glob("templates/*.html")
	control := regexp.MustCompile(`<(input|textarea|select)\b[^>]*>`)
	class := regexp.MustCompile(`class="([^"]*)"`)
	small := regexp.MustCompile(`\btext-(xs|sm|\[(1[0-5]|[0-9])px\])\b`)
	checked := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, tag := range control.FindAllString(string(b), -1) {
			if strings.Contains(tag, `type="hidden"`) || regexp.MustCompile(`\shidden[\s>]`).MatchString(tag) ||
				strings.Contains(tag, `type="checkbox"`) || strings.Contains(tag, `type="radio"`) {
				continue
			}
			checked++
			cls := ""
			if m := class.FindStringSubmatch(tag); m != nil {
				cls = " " + m[1] + " "
			}
			switch {
			case small.MatchString(cls):
				t.Errorf("%s: a control smaller than 16px: %s", f, tag)
			case !strings.Contains(cls, " input ") && !strings.Contains(cls, " text-base "):
				t.Errorf("%s: a control without .input or text-base: %s", f, tag)
			}
		}
	}
	if checked < 20 {
		t.Fatalf("only %d controls found", checked)
	}
}

// TestInteractiveTextAtLeast16px: iOS reports a link's or label's hit area
// as the frame of its text, so Apple's accessibility audit ("Hit area is
// too small") flags text under 16px inside links, buttons, summaries and
// labels even when the box is 44px tall (DESIGN §17.9). Their text stays
// at 16px or more; .btn and .chip are text-base.
func TestInteractiveTextAtLeast16px(t *testing.T) {
	small := regexp.MustCompile(`\btext-(xs|sm|\[(1[0-5]|[0-9])px\]|\[0\.\d+rem\])\b`)
	tags := regexp.MustCompile(`<(a|button|summary|label)\b[^>]*class="([^"]*)"`)
	files, _ := filepath.Glob("templates/*.html")
	checked := 0
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, m := range tags.FindAllStringSubmatch(string(b), -1) {
			checked++
			if small.MatchString(m[2]) {
				t.Errorf("%s: <%s> text under 16px: %s", f, m[1], m[2])
			}
		}
	}
	// Script-built controls: buttons and links made with a btn/chip class or
	// a className on an <a>.
	js, _ := filepath.Glob("static/js/*.js")
	built := regexp.MustCompile(`"(?:btn|chip)\b[^"]*"|\ba\.className = "[^"]*"|facts\.className = "[^"]*"`)
	for _, f := range js {
		b, _ := os.ReadFile(f)
		for _, s := range built.FindAllString(string(b), -1) {
			checked++
			if small.MatchString(s) {
				t.Errorf("%s: a control's text under 16px: %s", f, s)
			}
		}
	}
	css, _ := os.ReadFile("css/input.css")
	for _, c := range []string{".btn", ".chip"} {
		rule := regexp.MustCompile(regexp.QuoteMeta(c) + ` \{\s*@apply ([^;]*);`).FindSubmatch(css)
		if rule == nil || !strings.Contains(" "+string(rule[1])+" ", " text-base ") {
			t.Errorf("%s is not text-base", c)
		}
	}
	if checked < 50 {
		t.Fatalf("only %d interactive elements found", checked)
	}
}
