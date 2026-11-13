package voices

import (
	"context"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Stamp identifies the stored custom voice set; the warm speech server
// restarts when it changes, since it loads voices at launch.
func Stamp(ctx context.Context, st *store.Store) string {
	voices, err := st.Voices(ctx)
	if err != nil {
		return "error"
	}
	var b strings.Builder
	for _, v := range voices {
		b.WriteString(v.ID + ":" + v.SHA256 + ";")
	}
	return b.String()
}
