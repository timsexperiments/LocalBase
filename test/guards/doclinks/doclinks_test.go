// Package doclinks checks the repository's Markdown links: every relative link in the top's .md
// files and under docs/ (docs/site aside: its links are to the pages the site build assembles)
// names a file or directory that exists. Web links and in-page anchors
// aren't checked. The root move (M1) is what it was written for: moving a tree breaks the links
// that pointed into it, and a link checker finds them before a reader does.
package doclinks

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/internal/testutil"
)

// minPages is how many pages were checked when it was set. A walk that stops reading fails.
const minPages = 58

// link is [text](target) or ![alt](target), the target up to whitespace or the closing parenthesis.
var link = regexp.MustCompile(`!?\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)

// fence is a fenced code block's delimiter line; links inside code aren't links.
var fence = regexp.MustCompile("^\\s*(```|~~~)")

func TestRelativeLinksResolve(t *testing.T) {
	root := testutil.RepoRoot(t)
	var pages []string
	tops, _ := filepath.Glob(filepath.Join(root, "*.md"))
	pages = append(pages, tops...)
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "site" && filepath.Dir(p) == filepath.Join(root, "docs") {
			return filepath.SkipDir // the docs site's own pages link to pages scripts/docs-site.sh assembles
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") {
			pages = append(pages, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) < minPages {
		t.Fatalf("read %d pages, fewer than the %d there were: a tree stopped being read", len(pages), minPages)
	}
	var broken []string
	for _, page := range pages {
		broken = append(broken, brokenIn(t, root, page)...)
	}
	sort.Strings(broken)
	for _, b := range broken {
		t.Error(b)
	}
	t.Logf("checked %d pages", len(pages))
}

// brokenIn lists the relative links in page whose target doesn't exist, as "page:line: target".
func brokenIn(t *testing.T, root, page string) []string {
	t.Helper()
	b, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(root, page)
	var out []string
	inFence := false
	for n, line := range strings.Split(string(b), "\n") {
		if fence.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		for _, m := range link.FindAllStringSubmatch(stripCode(line), -1) {
			target := m[1]
			if target == "" || strings.HasPrefix(target, "#") || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			if i := strings.IndexAny(target, "#?"); i >= 0 {
				target = target[:i]
			}
			if unescaped, err := url.PathUnescape(target); err == nil {
				target = unescaped
			}
			resolved := filepath.Join(filepath.Dir(page), target)
			if strings.HasPrefix(target, "/") {
				resolved = filepath.Join(root, target)
			}
			if _, err := os.Stat(resolved); err != nil {
				out = append(out, rel+":"+itoa(n+1)+": "+m[1])
			}
		}
	}
	return out
}

// stripCode removes inline code spans, where brackets and parentheses aren't links.
func stripCode(line string) string {
	var b strings.Builder
	in := false
	for _, r := range line {
		if r == '`' {
			in = !in
			continue
		}
		if !in {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}
