// Command catalog-feed writes the remote catalog feeds gateways download,
// catalog/feed.json (v1, models only) and catalog/feed-v2.json (models and
// annotations), from the catalog data in internal/catalog/data and the memory
// measurements in internal/memory/measurements.json.
//
//	cd go && go run ./tools/catalog-feed         # write both feeds
//	cd go && go run ./tools/catalog-feed -check  # exit 1 if either is out of date
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

const module = "github.com/timsexperiments/LocalBase/go"

// moduleDir is the directory of this module's go.mod, the nearest one at or
// above the working directory.
func moduleDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			first, _, _ := strings.Cut(string(b), "\n")
			if strings.TrimSpace(first) == "module "+module {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("run from inside the go/ module (no go.mod for " + module + " above the working directory)")
		}
		dir = parent
	}
}

func main() {
	check := flag.Bool("check", false, "exit non-zero when a checked-in feed is out of date, writing nothing")
	flag.Parse()
	if err := run(*check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(check bool) error {
	dir, err := moduleDir()
	if err != nil {
		return err
	}
	root := filepath.Dir(dir)
	v1, v2, err := catalog.RenderFeeds(memory.MeasurementsJSON())
	if err != nil {
		return err
	}
	outputs := []struct {
		rel  string
		body []byte
	}{{"catalog/feed.json", v1}, {"catalog/feed-v2.json", v2}}
	for _, o := range outputs {
		path := filepath.Join(root, filepath.FromSlash(o.rel))
		if check {
			current, _ := os.ReadFile(path)
			if !bytes.Equal(current, o.body) {
				return fmt.Errorf("%s is out of date. Run `cd go && go run ./tools/catalog-feed`.", o.rel)
			}
			continue
		}
		if err := os.WriteFile(path, o.body, 0o644); err != nil {
			return err
		}
		fmt.Printf("Wrote %s.\n", path)
	}
	return nil
}
