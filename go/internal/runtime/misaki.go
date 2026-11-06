package runtime

import (
	"errors"
	"fmt"
	"os"

	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
)

// MergeMisaki builds CrispASR's Kokoro G2P dictionary from the pinned misaki
// us_gold.json and us_silver.json: every gold entry, then silver entries gold
// lacks, written as compact JSON with a trailing newline. It reproduces the
// file the 2026-10-20 TTS screen used byte for byte (JavaScript object
// semantics: { ...gold } plus missing silver keys, JSON.stringify).
func MergeMisaki(goldPath, silverPath string) ([]byte, error) {
	read := func(p string) (*jsjson.Object, error) {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		v, err := jsjson.Parse(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		o, ok := v.(*jsjson.Object)
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
	merged := jsjson.NewObject()
	for _, k := range gold.Keys() {
		v, _ := gold.Get(k)
		merged.Set(k, v)
	}
	for _, k := range silver.Keys() {
		if _, ok := merged.Get(k); !ok {
			v, _ := silver.Get(k)
			merged.Set(k, v)
		}
	}
	return append(jsjson.Stringify(merged), '\n'), nil
}
