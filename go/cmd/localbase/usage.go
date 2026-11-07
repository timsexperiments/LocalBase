package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/timsexperiments/LocalBase/go/internal/service"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
)

// usageNote is the one line that says what this is, here and on the page (DESIGN §32.1).
const usageNote = "Counts only, stored on this computer, never prompts or replies; turn off or clear any time"

func usageCommand() *cli.Command {
	return &cli.Command{Name: "usage", Summary: "Show requests, tokens, errors and latency",
		Options: []cli.Option{
			{Name: "range", Value: "<24h|7d|30d>", Help: "How far back; defaults to 7d"},
			{Name: "from", Value: "<time>", Help: "Start, RFC 3339, instead of --range"},
			{Name: "to", Value: "<time>", Help: "End, RFC 3339, instead of --range"},
			{Name: "by", Value: "<key|model|route>", Repeatable: true, Help: "Break down by this"},
			{Name: "key", Value: "<id>", Help: "One key or person"},
			{Name: "model", Value: "<id>", Help: "One model"},
			{Name: "route", Value: "<route>", Help: "One route"},
		},
		Examples: []string{"local-base usage", "local-base usage --range 30d --by key --by model", "local-base usage clear --yes"},
		Run:      withEnv(runUsage),
		Sub: []*cli.Command{
			{Name: "clear", Summary: "Remove the totals", Options: []cli.Option{
				{Name: "before", Value: "<time>", Help: "Only what is older than this, RFC 3339"},
				{Name: "yes", Alias: "y", Bool: true, Help: "Confirm"},
			}, Run: withEnv(runUsageClear)},
			{Name: "retention", Summary: "Keep totals for this many days", Usage: "<days>", MinArgs: 1, MaxArgs: 1, Run: withEnv(runUsageRetention)},
			{Name: "recording", Summary: "Turn counting on or off", Usage: "<on|off>", MinArgs: 1, MaxArgs: 1, Run: withEnv(runUsageRecording)},
		}}
}

func runUsage(c *cli.Ctx, e *env) (any, error) {
	groups := []string{}
	for _, by := range c.Strings("by") {
		switch by {
		case "key":
			groups = append(groups, "principal")
		case "model", "route":
			groups = append(groups, by)
		default:
			return nil, cli.Inputf("by: expected key, model or route")
		}
	}
	if r := c.String("route"); r != "" && !slices.Contains(usage.Routes, r) {
		return nil, cli.Inputf("route: expected one of %s", strings.Join(usage.Routes, ", "))
	}
	from, to, err := usage.ParseRange(c.String("range"), c.String("from"), c.String("to"), time.Now())
	if err != nil {
		return nil, cli.Inputf("%s", err.Error())
	}
	rec, err := usage.New(e.Store, nil)
	if err != nil {
		return nil, err
	}
	report, err := rec.Report(context.Background(), usage.Query{From: from, To: to, Principal: c.String("key"), Model: c.String("model"), Route: c.String("route"), Group: groups})
	if err != nil {
		return nil, err
	}
	report.Scope = "all"
	if !c.Global.JSON {
		if err := printUsage(c, report); err != nil {
			return nil, err
		}
	}
	return report, nil
}

func printUsage(c *cli.Ctx, r *usage.Report) error {
	if r.Totals.Requests == 0 {
		c.Info("No requests in this period.")
	} else {
		t := r.Totals
		c.Info("%s requests, %s errors (%.1f%%), %s cancelled", count(t.Requests), count(t.Failed+t.Refused), t.ErrorRate*100, count(t.Cancelled))
		c.Info("Tokens: %s in, %s out, from the %s requests that reported them", count(t.InputTokens), count(t.OutputTokens), count(t.RequestsWithTokens))
		if t.Latency.P50 != nil {
			c.Info("Latency, approximately: %s p50, %s p95, %s p99", ms(*t.Latency.P50), ms(*t.Latency.P95), ms(*t.Latency.P99))
		}
	}
	if len(r.Groups) > 0 {
		c.Info("")
		w := tabwriter.NewWriter(c.Stdout, 0, 2, 2, ' ', 0)
		if c.Global.JSON {
			w = tabwriter.NewWriter(c.Stderr, 0, 2, 2, ' ', 0)
		}
		fmt.Fprintln(w, "WHO\tMODEL\tROUTE\tREQUESTS\tERRORS\tTOKENS IN\tTOKENS OUT\tP95")
		for _, g := range r.Groups {
			who, model, route := "-", "-", "-"
			if p := g.Principal; p != nil {
				who = principalLabel(p)
			}
			if g.Model != nil && *g.Model != "" {
				model = *g.Model
			}
			if g.Route != nil {
				route = *g.Route
			}
			p95 := "-"
			if g.Latency.P95 != nil {
				p95 = ms(*g.Latency.P95)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", cli.Printable(who), cli.Printable(model), cli.Printable(route), count(g.Requests), count(g.Failed+g.Refused), count(g.InputTokens), count(g.OutputTokens), p95)
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("could not write the usage table: %w", err)
		}
	}
	c.Info("")
	state := "on"
	if !r.Recording {
		state = "off"
	}
	c.Info("Recording is %s. Totals are kept for %d days.", state, r.RetentionDays)
	c.Info("%s.", usageNote)
	return nil
}

// principalLabel is a key's name and id, or an opaque id: there is no name for a browser
// identity, because none was stored.
func principalLabel(p *usage.Principal) string {
	switch {
	case p.Name != "":
		return p.Name + " (" + p.ID + ")"
	case len(p.ID) > 20:
		return p.ID[:20] + "…"
	}
	return p.ID
}

func count(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func ms(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.1f s", v/1000)
	}
	return fmt.Sprintf("%.0f ms", v)
}

func runUsageClear(c *cli.Ctx, e *env) (any, error) {
	if !c.Bool("yes") {
		return nil, cli.Inputf("clear removes usage totals. Re-run with --yes to confirm.")
	}
	body := map[string]any{"action": "clear"}
	var before *time.Time
	if v := c.String("before"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, cli.Inputf("before: expected an RFC 3339 time")
		}
		before, body["before"] = &t, v
	}
	out, err := usageWrite(e, body, func(r *usage.Recorder) error { return r.Clear(context.Background(), before) })
	if err != nil {
		return nil, err
	}
	c.Info("Cleared the usage totals.")
	return out, nil
}

func runUsageRetention(c *cli.Ctx, e *env) (any, error) {
	days, err := strconv.Atoi(c.Args[0])
	if err != nil {
		return nil, cli.Inputf("days: expected a whole number")
	}
	if err := (&usage.InvalidRetention{Days: days}); days < 1 || days > 365 {
		return nil, cli.Inputf("%s", err.Error())
	}
	out, err := usageWrite(e, map[string]any{"action": "set-retention", "days": days}, func(r *usage.Recorder) error { return r.SetRetention(context.Background(), days) })
	if err != nil {
		return nil, err
	}
	c.Info("Usage totals are now kept for %d days.", days)
	return out, nil
}

func runUsageRecording(c *cli.Ctx, e *env) (any, error) {
	var on bool
	switch c.Args[0] {
	case "on":
		on = true
	case "off":
	default:
		return nil, cli.Inputf("recording: expected on or off")
	}
	out, err := usageWrite(e, map[string]any{"action": "set-recording", "enabled": on}, func(r *usage.Recorder) error { return r.SetRecording(context.Background(), on) })
	if err != nil {
		return nil, err
	}
	c.Info("Usage recording is %s.", c.Args[0])
	return out, nil
}

// usageWrite is a usage change, through the gateway or to the database.
func usageWrite(e *env, body map[string]any, direct func(*usage.Recorder) error) (map[string]any, error) {
	return writeThroughGateway(e, "/_localbase/usage", body, func() (map[string]any, error) {
		rec, err := usage.New(e.Store, nil)
		if err != nil {
			return nil, err
		}
		if err := direct(rec); err != nil {
			return nil, err
		}
		action, _ := body["action"].(string)
		auditChange(e, "usage", action, "usage", "")
		recording, days := rec.Settings()
		return map[string]any{"recording": recording, "retentionDays": days, "cleared": body["action"] == "clear"}, nil
	})
}

// writeThroughGateway makes a change without going behind a running gateway's back (DESIGN §32.4):
// with one running, the change goes through it, and otherwise this refuses. It writes the database
// only when no gateway has the root.
func writeThroughGateway(e *env, route string, body map[string]any, direct func() (map[string]any, error)) (map[string]any, error) {
	state, owner, detail := service.VerifiedOwnerState(e.Root) // a write goes only to a proven gateway
	switch state {
	case "active":
		return throughGateway(e.Root.Dir, owner, route, body)
	case "missing", "stale":
		out, err := direct()
		if err != nil {
			return nil, err
		}
		out["via"] = "database"
		return out, nil
	}
	if detail == "" {
		detail = "its lock could not be read"
	}
	return nil, errors.New("Cannot tell whether a gateway is running on this root: " + detail + ". Nothing changed.")
}

// stopGatewayAdvice is for a change that needs the gateway stopped, since with none running it writes
// to the database directly. Stopping the service also disables it, so this says how to get it back. A
// gateway started with serve isn't the service, and `stop` refuses it, so this doesn't name that for one.
func stopGatewayAdvice(rootDir string, owner *root.Owner) string {
	start := " Nothing changed." + root.DefaultRootNote(rootDir)
	if owner != nil && owner.ServiceID == "" {
		return start + " Try again, or stop the gateway where it is running, with Ctrl-C or by ending that process. The service commands don't control it."
	}
	q := "'" + strings.ReplaceAll(rootDir, "'", `'\''`) + "'"
	return start + " Try again, or stop the gateway first: local-base --root " + q + " stop. That also disables the service, until you run: local-base --root " + q + " start"
}

func throughGateway(rootDir string, owner *root.Owner, route string, body map[string]any) (map[string]any, error) {
	healthURL, token := owner.HealthURL(), owner.InstanceToken
	stopIt := stopGatewayAdvice(rootDir, owner)
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(healthURL, "/health")+route, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Localbase-Instance-Token", token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, errors.New("A gateway is running on this root, but it did not answer." + stopIt)
	}
	defer resp.Body.Close()
	var out map[string]any
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil, fmt.Errorf("The gateway running on this root did not accept the change (HTTP %d). It may be an older build, or not reachable from this computer's loopback address.%s", resp.StatusCode, stopIt)
	}
	delete(out, "object")
	out["via"] = "gateway"
	return out, nil
}
