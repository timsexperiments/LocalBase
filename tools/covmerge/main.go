// Command covmerge merges Go coverage profiles and prints statement coverage
// per package: what scripts/coverage.sh reports. A block counts as covered when
// any profile covered it.
//
//	go run ./tools/covmerge -o merged.out unit.out contract.out
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

const module = "github.com/timsexperiments/LocalBase/go/"

type block struct{ stmts, hits int }

func main() {
	out := flag.String("o", "", "write the merged profile here")
	flag.Parse()
	blocks := map[string]block{}
	for _, name := range flag.Args() {
		if err := read(name, blocks); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if *out != "" {
		if err := write(*out, blocks); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	report(blocks)
}

// read adds a profile: lines of "file:start,end stmts count".
func read(name string, blocks map[string]block) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") {
			continue
		}
		var stmts, count int
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return fmt.Errorf("%s: bad line %q", name, line)
		}
		if _, err := fmt.Sscan(fields[1]+" "+fields[2], &stmts, &count); err != nil {
			return fmt.Errorf("%s: bad line %q", name, line)
		}
		b := blocks[fields[0]]
		b.stmts = stmts
		b.hits = max(b.hits, count)
		blocks[fields[0]] = b
	}
	return sc.Err()
}

func write(name string, blocks map[string]block) error {
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	fmt.Fprintln(w, "mode: set")
	for k, b := range blocks {
		fmt.Fprintf(w, "%s %d %d\n", k, b.stmts, min(b.hits, 1))
	}
	return w.Flush()
}

// excluded are vendored or test-only packages.
func excluded(pkg string) bool {
	for _, p := range []string{"third_party", "fakebackend", "placementtest", "testutil", "tools/"} {
		if strings.Contains(pkg, p) {
			return true
		}
	}
	return false
}

func report(blocks map[string]block) {
	type total struct{ stmts, covered int }
	pkgs := map[string]*total{}
	for k, b := range blocks {
		file, _, _ := strings.Cut(strings.TrimPrefix(k, module), ":")
		i := strings.LastIndex(file, "/")
		if i < 0 {
			continue
		}
		pkg := file[:i]
		if excluded(pkg) {
			continue
		}
		if pkgs[pkg] == nil {
			pkgs[pkg] = &total{}
		}
		pkgs[pkg].stmts += b.stmts
		if b.hits > 0 {
			pkgs[pkg].covered += b.stmts
		}
	}
	names := make([]string, 0, len(pkgs))
	for p := range pkgs {
		names = append(names, p)
	}
	sort.Strings(names)
	var all total
	for _, p := range names {
		t := pkgs[p]
		fmt.Printf("%-26s %5.1f%% %6d\n", p, 100*float64(t.covered)/float64(t.stmts), t.stmts)
		all.stmts += t.stmts
		all.covered += t.covered
	}
	fmt.Printf("%-26s %5.1f%% %6d\n", "TOTAL", 100*float64(all.covered)/float64(all.stmts), all.stmts)
}
