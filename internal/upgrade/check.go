package upgrade

import (
	"context"
	"log/slog"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The optional daily check (DESIGN §33.1). Off by default. GitHub allows 60 unauthenticated
// requests an hour per IP address, shared with everything behind it, so this makes at most one
// attempt in 24 hours, success or failure, kept across restarts. It sends the stored ETag, so an
// unchanged answer costs nothing against that limit. A failure is quiet and blocks nothing.

const (
	checkKey     = "update.check"
	latestKey    = "update.latest"
	urlKey       = "update.url"
	etagKey      = "update.etag"
	checkedAtKey = "update.checked-at"

	checkEvery = 24 * time.Hour
)

// Checker is the daily check's state.
type Checker struct {
	st      *store.Store
	src     Source
	log     *slog.Logger
	current string
	release bool
	now     func() time.Time
	// firstWait and tick are how soon and how often it looks at whether a check is due.
	firstWait, tick time.Duration
}

// NewChecker makes the check for a running version. release is false for a development build,
// which never checks.
func NewChecker(st *store.Store, src Source, current string, release bool, log *slog.Logger) *Checker {
	return &Checker{st: st, src: src, log: log, current: current, release: release, now: time.Now, firstWait: time.Minute, tick: time.Hour}
}

// State is what is known, without asking anyone.
type State struct {
	Object          string  `json:"object"`
	Enabled         bool    `json:"enabled"`
	Current         string  `json:"current"`
	Latest          *string `json:"latest"`
	URL             *string `json:"url"`
	CheckedAt       *string `json:"checkedAt"`
	UpdateAvailable bool    `json:"updateAvailable"`
}

func (c *Checker) setting(ctx context.Context, key string) string {
	v, _, err := c.st.Setting(ctx, key)
	if err != nil {
		return ""
	}
	return v
}

func optional(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// Enabled says whether the daily check is on. Anything but an explicit "on" is off.
func (c *Checker) Enabled(ctx context.Context) bool { return c.setting(ctx, checkKey) == "on" }

// SetEnabled turns the daily check on or off. Turning it on does not itself make a request.
func (c *Checker) SetEnabled(ctx context.Context, on bool) error {
	value := "off"
	if on {
		value = "on"
	}
	return c.st.SetSetting(ctx, checkKey, value)
}

// Read says what is stored. It asks nobody.
func (c *Checker) Read(ctx context.Context) State {
	latest := c.setting(ctx, latestKey)
	s := State{Object: "localbase.update", Enabled: c.Enabled(ctx), Current: c.current, Latest: optional(latest), URL: optional(c.setting(ctx, urlKey)),
		CheckedAt: optional(c.setting(ctx, checkedAtKey))}
	s.UpdateAvailable = c.release && s.Enabled && Compare(c.current, latest).Newer
	return s
}

// CheckIfDue makes the request if the check is on and 24 hours have passed. It reports whether
// it asked. A failure stays here: it is logged, and the cached answer stands.
func (c *Checker) CheckIfDue(ctx context.Context) bool {
	if !c.release || !c.Enabled(ctx) {
		return false
	}
	if at, err := time.Parse(time.RFC3339, c.setting(ctx, checkedAtKey)); err == nil && c.now().Sub(at) < checkEvery {
		return false
	}
	// Recorded first: whatever happens next, it does not ask again for a day.
	if err := c.st.SetSetting(ctx, checkedAtKey, c.now().UTC().Format(time.RFC3339)); err != nil {
		c.log.Warn("Could not record the update check.", "event", "update.check-failed", "error", err.Error())
		return false
	}
	latest, err := c.src.Latest(ctx, c.setting(ctx, etagKey))
	if err != nil {
		c.log.Info("Could not check for a newer version. It will try again tomorrow.", "event", "update.check-failed", "error", err.Error())
		return true
	}
	if latest.NotModified {
		return true
	}
	// Only a release counts. A runtime package's tag leaves the answer as it was.
	if !Compare("0.0.0", latest.Tag).Known {
		return true
	}
	for key, value := range map[string]string{latestKey: latest.Tag, urlKey: latest.URL, etagKey: latest.ETag} {
		if err := c.st.SetSetting(ctx, key, value); err != nil {
			c.log.Warn("Could not record the update check.", "event", "update.check-failed", "error", err.Error())
		}
	}
	return true
}

// Start looks in the background, so it cannot delay startup or a request. The returned function
// stops it.
func (c *Checker) Start() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait := time.NewTimer(c.firstWait)
		defer wait.Stop()
		select {
		case <-wait.C:
		case <-ctx.Done():
			return
		}
		tick := time.NewTicker(c.tick)
		defer tick.Stop()
		for {
			c.CheckIfDue(ctx)
			select {
			case <-tick.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	return func() { cancel(); <-done }
}
