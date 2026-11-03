package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/usage"
)

// Usage and activity (DESIGN §32.8): hourly totals, read and changed through
// /_localbase/usage as the signed-in person, so the API's scopes apply. The page shows
// everyone their own, and offers more only to those allowed it.

// usageNoteLine says what this is, on every state of the page (DESIGN §32.1).
const usageNoteLine = "Counts only, stored on this computer, never prompts or replies; turn off or clear any time"

type usageChoice struct {
	Value, Label string
	URL          string
	Current      bool
}

type usageRow struct {
	Who, Model, Route string
	Stats             usageFigures
}

// usageFigures are one level's numbers, ready to show.
type usageFigures struct {
	Requests, Errors, ErrorRate, Cancelled string
	InputTokens, OutputTokens              string
	P50, P95, P99                          string
	PartialTokens                          bool
	WithTokens                             string
}

func figures(s usage.Stats) usageFigures {
	f := usageFigures{Requests: groupDigits(s.Requests), Errors: groupDigits(s.Failed + s.Refused), Cancelled: groupDigits(s.Cancelled),
		ErrorRate: fmt.Sprintf("%.1f%%", s.ErrorRate*100), InputTokens: groupDigits(s.InputTokens), OutputTokens: groupDigits(s.OutputTokens),
		PartialTokens: s.RequestsWithTokens < s.Requests, WithTokens: groupDigits(s.RequestsWithTokens), P50: "n/a", P95: "n/a", P99: "n/a"}
	if s.Latency.P50 != nil {
		f.P50, f.P95, f.P99 = approxMs(*s.Latency.P50), approxMs(*s.Latency.P95), approxMs(*s.Latency.P99)
	}
	return f
}

func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// approxMs marks a percentile as what it is: within about 12%.
func approxMs(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("~%.1f s", v/1000)
	}
	return fmt.Sprintf("~%.0f ms", v)
}

func (u *UI) usagePage(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "usage", u.usageData(r, "", ""))
}

func (u *UI) usageData(r *http.Request, notice, failure string) map[string]any {
	p := u.caller(r)
	canSeeAll := u.d.NoAuth || usage.CanSeeAll(p.Can)
	canManage := u.d.NoAuth || usage.CanChange(p.Can)
	rng := r.FormValue("range")
	if !slices.Contains([]string{"24h", "7d", "30d"}, rng) {
		rng = "7d"
	}
	scope := r.FormValue("scope")
	if scope != "all" || !canSeeAll {
		scope = "self"
	}
	group := r.FormValue("group")
	allowed := []string{"model", "route"}
	if scope == "all" {
		allowed = append(allowed, "principal")
	}
	if !slices.Contains(allowed, group) {
		group = ""
	}
	link := func(rngV, scopeV, groupV string) string {
		q := url.Values{"range": {rngV}, "scope": {scopeV}}
		if groupV != "" {
			q.Set("group", groupV)
		}
		return "/app/usage?" + q.Encode()
	}
	choices := func(values, labels []string, current string, url func(string) string) []usageChoice {
		out := make([]usageChoice, 0, len(values))
		for i, v := range values {
			out = append(out, usageChoice{v, labels[i], url(v), v == current})
		}
		return out
	}
	data := map[string]any{"Note": usageNoteLine, "Scope": scope, "CanSeeAll": canSeeAll, "CanManage": canManage, "Range": rng, "Group": group,
		"Ranges": choices([]string{"24h", "7d", "30d"}, []string{"24 hours", "7 days", "30 days"}, rng, func(v string) string { return link(v, scope, group) }),
		"Scopes": choices([]string{"self", "all"}, []string{"Yours", "Everything"}, scope, func(v string) string { return link(rng, v, "") }),
		"Notice": notice, "Error": failure}
	groupValues, groupLabels := []string{"", "model", "route"}, []string{"Total", "Model", "Route"}
	if scope == "all" {
		groupValues, groupLabels = []string{"", "principal", "model", "route"}, []string{"Total", "Who", "Model", "Route"}
	}
	data["Groups"] = choices(groupValues, groupLabels, group, func(v string) string { return link(rng, scope, v) })

	q := url.Values{"range": {rng}, "scope": {scope}}
	if group != "" {
		q.Set("group", group)
	}
	res := u.call(r, "GET", "/_localbase/usage?"+q.Encode(), "", nil)
	var report usage.Report
	if res.Status != http.StatusOK || json.Unmarshal(res.Body, &report) != nil {
		msg := res.apiError()
		if msg == "" {
			msg = "Could not read usage. Try again."
		}
		data["LoadError"] = msg
		return data
	}
	data["Usage"] = report
	data["Recording"] = report.Recording
	data["RetentionDays"] = report.RetentionDays
	data["Empty"] = report.Totals.Requests == 0
	data["Totals"] = figures(report.Totals)
	rows := make([]usageRow, 0, len(report.Groups))
	for _, g := range report.Groups {
		row := usageRow{Stats: figures(g.Stats)}
		if g.Principal != nil {
			row.Who = usageWho(g.Principal)
		}
		if g.Model != nil {
			row.Model = *g.Model
			if row.Model == "" {
				row.Model = "No model named"
			}
		}
		if g.Route != nil {
			row.Route = *g.Route
		}
		rows = append(rows, row)
	}
	data["Rows"] = rows
	return data
}

// usageWho is a key's name, or an opaque id. A browser identity has no name here, because
// none is stored (DESIGN §32.1).
func usageWho(p *usage.Principal) string {
	switch {
	case p.Name != "":
		return p.Name
	case p.Kind == "browser":
		id := strings.TrimPrefix(p.ID, "browser:")
		if len(id) > 8 {
			id = id[:8]
		}
		return "Signed-in person " + id
	case p.Kind == "local":
		return "This computer"
	}
	return p.ID
}

// usageChange sends a change to the API, which applies its own permission check.
func (u *UI) usageChange(w http.ResponseWriter, r *http.Request) {
	f := r.PostFormValue
	var body map[string]any
	notice := ""
	switch f("action") {
	case "clear":
		body, notice = map[string]any{"action": "clear"}, "Usage totals cleared."
	case "retention":
		days, err := strconv.Atoi(strings.TrimSpace(f("days")))
		if err != nil {
			r.Header.Set("HX-Request", "true")
			r.Header.Set("HX-Target", "usage-body")
			u.render(w, r, "usage", u.usageData(r, "", "Keep totals for a whole number of days."))
			return
		}
		body, notice = map[string]any{"action": "set-retention", "days": days}, "Saved. Totals are kept for "+strconv.Itoa(days)+" days."
	case "recording":
		on := f("enabled") == "on"
		body = map[string]any{"action": "set-recording", "enabled": on}
		notice = "Recording is off. Nothing new is counted, and what exists stays until you clear it or it ages out."
		if on {
			notice = "Recording is on."
		}
	default:
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	failure := u.apiCall(r, "POST", "/_localbase/usage", body, nil)
	if failure != "" {
		notice = ""
	}
	r.Header.Set("HX-Request", "true")
	r.Header.Set("HX-Target", "usage-body")
	u.render(w, r, "usage", u.usageData(r, notice, failure))
}
