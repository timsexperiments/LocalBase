package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// Model import, backend selection and backend registration pages (DESIGN
// §12.6). Detection findings are labels and warnings: every registered
// backend stays selectable.

var ratingLabels = map[string]string{
	backends.Supported:    "Supported",
	backends.Likely:       "Likely to work",
	backends.Unknown:      "Support unknown",
	backends.Incompatible: "Detected as incompatible",
}

var installLabels = map[string]string{
	backends.Installed:   "installed",
	backends.Installable: "installs on first use",
	backends.Missing:     "not installed here",
}

type optionView struct {
	ID, Name, Rating, RatingLabel, Install, Reason string
	Warnings                                       []string
	Selected, Recommended                          bool
}

type backendView struct {
	ModelID       string
	Mode          string
	BackendID     string
	BackendName   string
	Unavailable   bool
	Rating        string
	RatingLabel   string
	Reasons       []string
	Warnings      []string
	DownloadBytes int64
	Recommended   string
	Options       []optionView
	Message       string
	Error         string
}

func options(reg *backends.Registry, h backends.Host, rec backends.Recommendation, selected string) []optionView {
	var out []optionView
	for _, o := range rec.Options {
		v := optionView{ID: o.BackendID, Name: o.BackendID, Rating: o.Rating, RatingLabel: ratingLabels[o.Rating],
			Install: h.InstallLabel(o.BackendID), Selected: o.BackendID == selected, Recommended: o.BackendID == rec.BackendID,
			Warnings: backends.Messages(o.Warnings)}
		if d := reg.Get(o.BackendID); d != nil {
			v.Name = d.Name
		}
		if len(o.Reasons) > 0 {
			v.Reason = o.Reasons[0].Message
		}
		out = append(out, v)
	}
	return out
}

func (u *UI) backendView(modelID string, s backends.Selection) backendView {
	reg := u.d.Library.Registry
	v := backendView{ModelID: modelID, Mode: s.Mode, BackendID: s.BackendID, Unavailable: s.Unavailable,
		DownloadBytes: s.DownloadBytes, Options: options(reg, u.d.Library.Host(), s.Recommendation, s.BackendID)}
	if d := reg.Get(s.BackendID); d != nil {
		v.BackendName = d.Name
	}
	if r := s.Recommendation.BackendID; r != "" {
		v.Recommended = r
		if d := reg.Get(r); d != nil {
			v.Recommended = d.Name
		}
	}
	if f := s.Finding; f != nil {
		v.Rating, v.RatingLabel, v.Reasons, v.Warnings = f.Rating, ratingLabels[f.Rating], backends.Messages(f.Reasons), backends.Messages(f.Warnings)
	}
	return v
}

// mutating guards a state-changing form post: an htmx request from this
// page's own origin, carrying the UI marker header.
// sameOriginUI accepts only htmx requests from this origin with the UI header.
func sameOriginUI(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	origin := r.Header.Get("Origin")
	sameOrigin := origin == ""
	if o, err := url.Parse(origin); err == nil && origin != "" {
		sameOrigin = o.Host == r.Host
	}
	return r.Header.Get("X-Localbase-Ui") == "1" && r.Header.Get("HX-Request") == "true" && sameOrigin &&
		(site == "" || site == "same-origin")
}

func (u *UI) mutating(permission string, next http.HandlerFunc) http.HandlerFunc {
	return u.need(permission, func(w http.ResponseWriter, r *http.Request) {
		if !sameOriginUI(r) || !u.csrfValid(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		next(w, r)
	})
}

// --- model settings: backend ------------------------------------------------------

func (u *UI) backendPanel(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/app/models/m/"+url.PathEscape(r.PathValue("id")), http.StatusSeeOther)
		return
	}
	l := u.d.Library
	id := r.PathValue("id")
	ctx := r.Context()
	var sel backends.Selection
	var err error
	msg := ""
	switch {
	case r.Method == http.MethodPost && r.FormValue("reset") != "":
		err = root.WithOperation(u.d.RootDir, "update runtime configuration", func() (err error) {
			sel, err = l.ResetBackend(ctx, id)
			return err
		})
		msg = "Reset to automatic selection."
	case r.Method == http.MethodPost:
		b := r.FormValue("backend")
		if b == "" {
			err = errors.New("choose a backend")
			break
		}
		err = root.WithOperation(u.d.RootDir, "update runtime configuration", func() (err error) {
			sel, err = l.SetBackend(ctx, id, b, "")
			return err
		})
		msg = "Saved for this device. Detection will not change it."
	default:
		var e *library.Entry
		if e, err = l.Get(ctx, id); err == nil {
			sel, err = l.Selection(ctx, e)
		}
	}
	if errors.Is(err, library.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	v := u.backendView(id, sel)
	if err != nil {
		v = backendView{ModelID: id, Error: err.Error()}
	} else {
		v.Message = msg
	}
	r.Header.Set("HX-Target", "backend-panel-frag")
	u.render(w, r, "details", map[string]any{"Backend": v})
}

// --- import -------------------------------------------------------------------------

type importForm struct {
	Source, File, Task, AuthProfile, SHA256, ID, Backend string
	AllowHTTP, Confirm                                   bool
}

func readImportForm(r *http.Request) importForm {
	return importForm{Source: strings.TrimSpace(r.FormValue("source")), File: strings.TrimSpace(r.FormValue("file")),
		Task: r.FormValue("task"), AuthProfile: r.FormValue("auth_profile"), SHA256: strings.TrimSpace(r.FormValue("sha256")),
		ID: strings.TrimSpace(r.FormValue("id")), Backend: r.FormValue("backend"),
		AllowHTTP: r.FormValue("allow_http") == "1", Confirm: r.FormValue("confirm") == "1"}
}

func (u *UI) profileNames() []string {
	ps, err := acquire.LoadProfiles(u.d.RootDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range ps.Profiles {
		out = append(out, p.ID)
	}
	return out
}

func (u *UI) importPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "import", map[string]any{"Tasks": models.Tasks, "Profiles": u.profileNames(), "Form": importForm{}})
}

func (u *UI) resolve(r *http.Request, f importForm) (*acquire.Plan, error) {
	req := acquire.Request{Source: f.Source, Revision: "", SHA256: f.SHA256, AuthProfile: f.AuthProfile, AllowHTTP: f.AllowHTTP,
		ID: f.ID, Task: f.Task}
	if f.File != "" {
		req.Files = []string{f.File}
	}
	if cfg, _ := u.d.Store.LoadConfig(r.Context()); cfg != nil {
		req.HFToken = cfg.HFToken
	}
	if req.HFToken == "" {
		req.HFToken = os.Getenv("HF_TOKEN")
	}
	return acquire.Resolve(r.Context(), u.d.RootDir, req)
}

func (u *UI) importResolve(w http.ResponseWriter, r *http.Request) {
	f := readImportForm(r)
	data := map[string]any{"Form": f, "Tasks": models.Tasks}
	r.Header.Set("HX-Target", "import-plan")
	if f.Source == "" {
		data["Error"] = "Enter a Hugging Face repository, a URL or a file path."
		u.render(w, r, "import", data)
		return
	}
	plan, err := u.resolve(r, f)
	if err != nil {
		data["Error"] = obs.BoundedText(err.Error(), 512)
		u.render(w, r, "import", data)
		return
	}
	rec := u.d.Library.Preview(plan)
	data["Plan"] = plan
	data["Token"] = u.remember(f)
	data["Form"] = importForm{} // never echo the source or checksum back
	data["Insecure"] = slices.ContainsFunc(plan.Warnings, func(w acquire.Warning) bool { return w.Code == acquire.WarnInsecureHTTP })
	data["Options"] = options(u.d.Library.Registry, u.d.Library.Host(), rec, "")
	data["Recommended"] = rec.BackendID
	if d := u.d.Library.Registry.Get(rec.BackendID); d != nil {
		data["Recommended"] = d.Name
	}
	u.render(w, r, "import", data)
}

func (u *UI) importStart(w http.ResponseWriter, r *http.Request) {
	token := r.FormValue("token")
	f, ok := u.recall(token)
	if !ok {
		r.Header.Set("HX-Target", "import-plan")
		u.render(w, r, "import", map[string]any{"Error": "This check expired. Check the source again."})
		return
	}
	if v := strings.TrimSpace(r.FormValue("file")); v != "" {
		f.File = v
	}
	f.Backend, f.Confirm = r.FormValue("backend"), r.FormValue("confirm") == "1"
	r.Header.Set("HX-Target", "import-plan")
	data := map[string]any{"Tasks": models.Tasks}
	fail := func(msg string) {
		data["Error"] = msg
		u.render(w, r, "import", data)
	}
	plan, err := u.resolve(r, f)
	switch {
	case err != nil:
		fail(obs.BoundedText(err.Error(), 512))
		return
	case len(plan.Choices) > 0:
		fail("Choose one of the repository's files.")
		return
	case plan.Custom && !f.Confirm:
		fail("Confirm that you trust this custom source to download it.")
		return
	case plan.Task == "":
		fail("Choose what the model does.")
		return
	}
	if _, err := u.d.Library.Get(r.Context(), plan.ID); err == nil {
		fail("A model with id " + plan.ID + " already exists. Check the source again with another model id.")
		return
	}
	if f.Backend != "" && f.Backend != "auto" && u.d.Library.Registry.Get(f.Backend) == nil {
		fail("That backend is not registered.")
		return
	}
	u.forget(token)
	id := u.d.Library.StartImport(plan, f.Backend, nil)
	r.Header.Set("HX-Target", "import-job")
	u.render(w, r, "import", map[string]any{"Job": u.d.Library.Job(id)})
}

func (u *UI) importJob(w http.ResponseWriter, r *http.Request) {
	job := u.d.Library.Job(r.PathValue("job"))
	if job == nil {
		http.NotFound(w, r)
		return
	}
	data := map[string]any{"Job": job}
	if job.Selection != nil {
		data["Backend"] = u.backendView(job.Model.ID, *job.Selection)
	}
	r.Header.Set("HX-Target", "import-job")
	u.render(w, r, "import", data)
}

// --- backends -----------------------------------------------------------------------

func (u *UI) backendsPage(w http.ResponseWriter, r *http.Request) {
	l := u.d.Library
	data := map[string]any{"Host": l.Host(), "Skipped": l.Registry.Skipped(), "InstallLabels": installLabels}
	var rows []map[string]any
	for _, d := range l.Registry.All() {
		rows = append(rows, map[string]any{"Def": d, "Install": l.Host().InstallLabel(d.ID)})
	}
	data["Backends"] = rows
	if r.Method == http.MethodPost {
		r.Header.Set("HX-Target", "backend-add")
		d := &backends.Definition{}
		dec := json.NewDecoder(strings.NewReader(r.FormValue("definition")))
		dec.DisallowUnknownFields()
		err := dec.Decode(d)
		if err == nil {
			err = root.WithOperation(u.d.RootDir, "register backend", func() error { return backends.Add(u.d.RootDir, d) })
		}
		if err != nil {
			data["AddError"] = err.Error()
			data["Definition"] = r.FormValue("definition")
		} else {
			data["Added"] = d.ID
			w.Header().Set("HX-Refresh", "true")
		}
	}
	u.render(w, r, "backends", data)
}

// pendingImport is a resolved import waiting for confirmation. The page gets
// only its token, so a source URL with signed query values or a typed
// checksum is never sent back to the browser.
type pendingImport struct {
	form    importForm
	created time.Time
}

const pendingTTL = 30 * time.Minute

func (u *UI) remember(f importForm) string {
	u.pendingMu.Lock()
	defer u.pendingMu.Unlock()
	if u.pending == nil {
		u.pending = map[string]pendingImport{}
	}
	for k, p := range u.pending {
		if time.Since(p.created) > pendingTTL {
			delete(u.pending, k)
		}
	}
	token := root.NewID()
	u.pending[token] = pendingImport{form: f, created: time.Now()}
	return token
}

func (u *UI) recall(token string) (importForm, bool) {
	u.pendingMu.Lock()
	defer u.pendingMu.Unlock()
	p, ok := u.pending[token]
	if !ok || time.Since(p.created) > pendingTTL {
		return importForm{}, false
	}
	return p.form, true
}

func (u *UI) forget(token string) {
	u.pendingMu.Lock()
	delete(u.pending, token)
	u.pendingMu.Unlock()
}
