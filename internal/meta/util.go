package meta

import (
	"encoding/json"

	"github.com/timsexperiments/LocalBase/go/internal/collate"
)

func jsonUnmarshal(b []byte, v any) { _ = json.Unmarshal(b, v) }

// localeCompare is JavaScript's String.localeCompare.
func localeCompare(a, b string) int { return collate.Compare(a, b) }

// caseFold is localeCompare at base sensitivity: case is ignored entirely.
func caseFold(a, b string) int { return collate.Fold(a, b) }
