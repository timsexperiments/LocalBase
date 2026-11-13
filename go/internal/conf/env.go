package conf

import (
	"fmt"
	"os"
	"strconv"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// ApplyEnv applies LOCALBASE_HOST, LOCALBASE_PORT, LOCALBASE_STT_HOST, LOCALBASE_STT_PORT and
// LOCALBASE_CTX_SIZE to the in-memory config, as TS does for every command. They are never saved.
// A number that isn't one, or is out of range, is a *DocumentError: bad input.
func ApplyEnv(cfg *store.Config) error {
	str := func(name string, dst *string) {
		if v := os.Getenv(name); v != "" {
			*dst = v
		}
	}
	num := func(name string, dst *int, lo, hi int) error {
		v := os.Getenv(name)
		if v == "" {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return &DocumentError{fmt.Sprintf("%s: must be an integer from %d to %d", name, lo, hi)}
		}
		*dst = n
		return nil
	}
	str("LOCALBASE_HOST", &cfg.Host)
	str("LOCALBASE_STT_HOST", &cfg.SttHost)
	if err := num("LOCALBASE_PORT", &cfg.Port, 1, 65535); err != nil {
		return err
	}
	if err := num("LOCALBASE_STT_PORT", &cfg.SttPort, 1, 65535); err != nil {
		return err
	}
	return num("LOCALBASE_CTX_SIZE", &cfg.CtxSize, 2048, 2_147_483_647)
}
