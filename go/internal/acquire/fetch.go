package acquire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/idle"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

// Progress reports a transfer. It names files only, never URLs or headers.
type Progress struct {
	Phase string `json:"phase"` // download | verify | inspect | publish | done
	File  string `json:"file,omitempty"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"` // 0 = unknown
	// Note is something a person should be told, once: a phase's progress lines say nothing else.
	Note string `json:"note,omitempty"`
}

// What a person is told, in Progress.Note.
const (
	noteCannotResume = "This source can't be resumed, so the download started again."
	noteFetchedAgain = "A file did not match its checksum and was fetched again."
)

// ImportedDir is where an imported model's files live.
func ImportedDir(rootDir, id string) string { return filepath.Join(rootDir, "models", "imported", id) }

func stagingDir(rootDir string, p *Plan) string {
	sum := sha256.Sum256([]byte(p.Kind + "\x00" + p.Display + "\x00" + p.ID))
	return filepath.Join(rootDir, "models", ".staging", p.ID+"-"+hex.EncodeToString(sum[:6]))
}

// HashMismatchError means a trusted checksum did not match; nothing was
// published.
type HashMismatchError struct{ File, Expected, Actual, Trust string }

func (e *HashMismatchError) Error() string {
	return fmt.Sprintf("%s does not match its %s checksum (expected %s, got %s); it was not published", e.File, e.Trust, e.Expected, e.Actual)
}

// Result is a published import.
type Result struct {
	Model      models.Model
	Dir        string
	Inspection models.Inspection
}

// Import fetches and publishes in one step.
func Import(ctx context.Context, rootDir string, p *Plan, progress func(Progress)) (*Result, error) {
	f, err := Fetch(ctx, rootDir, p, progress)
	if err != nil {
		return nil, err
	}
	return f.Publish()
}

// Fetched is an import whose files are in staging, checked and waiting to be
// published.
type Fetched struct {
	staging, dest string
	model         models.Model
	inspection    models.Inspection
	progress      func(Progress)
}

// Publish makes a fetched import a model. Hold the root's operation lease for this,
// not for the download, which can take hours.
func (f *Fetched) Publish() (*Result, error) {
	f.progress(Progress{Phase: "publish"})
	if _, err := os.Stat(f.dest); err == nil {
		return nil, fmt.Errorf("a model with id %s is already imported; choose another id", f.model.ID)
	}
	if err := os.MkdirAll(filepath.Dir(f.dest), 0o700); err != nil {
		return nil, err
	}
	if err := os.Rename(f.staging, f.dest); err != nil {
		return nil, fmt.Errorf("publishing the files failed: %v", err)
	}
	f.progress(Progress{Phase: "done"})
	return &Result{Model: f.model, Dir: f.dest, Inspection: f.inspection}, nil
}

// Fetch downloads (or copies) the plan's files into staging, checks sizes and
// checksums, and inspects the result. Partial downloads resume. Nothing is executed
// and no backend is registered or installed.
func Fetch(ctx context.Context, rootDir string, p *Plan, progress func(Progress)) (*Fetched, error) {
	if progress == nil {
		progress = func(Progress) {}
	}
	if len(p.Files) == 0 {
		return nil, fmt.Errorf("choose which file to import (%s)", strings.Join(p.Choices, ", "))
	}
	if p.Task == "" {
		return nil, fmt.Errorf("the source does not say what the model does; choose a task (%s)", strings.Join(models.Tasks, ", "))
	}
	if !slices.Contains(models.Tasks, p.Task) {
		return nil, fmt.Errorf("unknown task %q", p.Task)
	}
	dest := ImportedDir(rootDir, p.ID)
	if _, err := os.Stat(dest); err == nil {
		return nil, fmt.Errorf("a model with id %s is already imported; choose another id", p.ID)
	}
	staging := stagingDir(rootDir, p)
	PruneStaging(rootDir, staging)
	if err := CheckSpace(rootDir, p); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return nil, err
	}
	red := redactorFor(p.profile)
	red.Add(p.hfToken)
	client := newClient(p.profile, p.hfToken, p.hfOrigins, p.allowPrivate)
	var arts []models.Artifact
	for _, f := range p.Files {
		final := filepath.Join(staging, f.Filename)
		if _, err := os.Stat(final); err != nil {
			partial := final + ".partial"
			if f.local != "" {
				if err := copyFile(ctx, f.local, partial, p.budget(staging, f.SizeBytes), func(n int64) {
					progress(Progress{Phase: "download", File: f.Filename, Done: n, Total: f.SizeBytes})
				}); err != nil {
					return nil, err
				}
			} else if err := download(ctx, client, f.fetchURL, partial, f.SizeBytes, red, p.budget(staging, f.SizeBytes), resumeRule{pinned: f.SHA256 != "", cannotResume: func() {
				progress(Progress{Phase: "download", File: f.Filename, Note: noteCannotResume})
			}}, func(n int64) {
				progress(Progress{Phase: "download", File: f.Filename, Done: n, Total: f.SizeBytes})
			}); err != nil {
				return nil, refusal(err, p.onHost)
			}
			if err := os.Rename(partial, final); err != nil {
				return nil, err
			}
		}
		info, err := os.Stat(final)
		if err != nil {
			return nil, err
		}
		if f.SizeBytes > 0 && info.Size() != f.SizeBytes {
			os.Remove(final)
			return nil, fmt.Errorf("%s is %d bytes, expected %d; it was not published", f.Filename, info.Size(), f.SizeBytes)
		}
		progress(Progress{Phase: "verify", File: f.Filename, Done: info.Size(), Total: info.Size()})
		sum, err := fileSHA256(final)
		if err != nil {
			return nil, err
		}
		art := models.Artifact{Filename: f.Filename, Role: f.Role, Required: f.Required, SizeBytes: info.Size(), Source: f.Source}
		switch {
		case f.SHA256 != "" && f.SHA256 != sum:
			os.Remove(final)
			return nil, &HashMismatchError{File: f.Filename, Expected: f.SHA256, Actual: sum, Trust: f.HashTrust}
		case f.SHA256 != "":
			art.SHA256, art.HashTrust = sum, f.HashTrust
		default:
			// Identity only: this proves which bytes were received, not who
			// published them.
			art.SHA256, art.HashTrust = sum, models.HashObserved
		}
		arts = append(arts, art)
	}
	progress(Progress{Phase: "inspect"})
	// Roles come from the headers, not the file names: a projector is a file
	// whose header says so.
	for i := range arts {
		ff := models.InspectFile(filepath.Join(staging, arts[i].Filename))
		if ff.Refused {
			os.Remove(filepath.Join(staging, arts[i].Filename))
			return nil, fmt.Errorf("%s was not published: %s", arts[i].Filename, ff.Problem)
		}
		switch {
		case ff.Role == models.RoleProjector:
			arts[i].Role = models.RoleProjector
		case arts[i].Role == models.RoleProjector:
			arts[i].Role = models.RoleCompanion
		}
	}
	v := models.Variant{ID: "default", Artifacts: arts}
	in := models.InspectVariant(v, staging)
	if len(in.Missing) > 0 {
		return nil, fmt.Errorf("the import is incomplete; missing: %s", strings.Join(in.Missing, ", "))
	}
	if in.Variant.Precision.Known() {
		v.ID = strings.ToLower(strings.ReplaceAll(in.Variant.Precision.Value, "_", "-"))
	}
	in.Variant.ID = v.ID
	m := models.Model{ID: p.ID, Name: p.Name, Tasks: []string{p.Task}, Capabilities: []string{}, Origin: "import",
		License: p.License, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if p.Kind != "url" {
		m.SourceURL = p.Display
	}
	if p.Task == "chat" && slices.ContainsFunc(arts, func(a models.Artifact) bool { return a.Role == models.RoleProjector }) {
		m.Capabilities = append(m.Capabilities, "vision")
	}
	m.Variants = []models.Variant{in.Variant}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &Fetched{staging: staging, dest: dest, model: m, inspection: in, progress: progress}, nil
}

var contentRangeRe = regexp.MustCompile(`^bytes (\d+)-(\d+)/(\d+|\*)$`)

// download fetches url into partial, resuming from its current size.
func download(ctx context.Context, client *http.Client, url, partial string, expected int64, red *Redactor, b *budget, rule resumeRule, progress func(int64)) error {
	for attempt := 0; attempt < 2; attempt++ {
		offset := int64(0)
		if info, err := os.Stat(partial); err == nil {
			offset = info.Size()
		}
		if expected > 0 && offset > expected {
			os.Remove(partial)
			offset = 0
		}
		if expected > 0 && offset == expected {
			clearValidator(partial)
			return nil
		}
		validator := readValidator(partial)
		if offset > 0 && !validator.usable() && !rule.pinned {
			// Nothing says this is still the object the partial began as, and no hash will catch it
			// if it isn't, so the two must not be joined.
			if validator != validatorNoticed && rule.cannotResume != nil {
				rule.cannotResume()
			}
			os.Remove(partial)
			offset = 0
			validator = validatorNoticed
			writeValidator(partial, validator)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if offset > 0 {
			req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
			if validator.usable() {
				req.Header.Set("If-Range", string(validator))
			}
		}
		resp, err := idle.Do(client, req, idle.Timeout())
		if err != nil {
			return redactErr(err, red)
		}
		flags := os.O_CREATE | os.O_WRONLY
		switch {
		case resp.StatusCode == 206 && offset > 0:
			m := contentRangeRe.FindStringSubmatch(resp.Header.Get("Content-Range"))
			if m == nil || m[1] != strconv.FormatInt(offset, 10) {
				resp.Body.Close()
				os.Remove(partial)
				continue // the server resumed from somewhere else: start over
			}
			if now := validatorOf(resp); validator.usable() && now.usable() && now != validator {
				// It ignored If-Range: these bytes belong to another object.
				resp.Body.Close()
				os.Remove(partial)
				clearValidator(partial)
				continue
			}
			flags |= os.O_APPEND
		case resp.StatusCode == 200:
			flags |= os.O_TRUNC // no resume support, or the object changed: start from zero
			offset = 0
			if now := validatorOf(resp); now.usable() || validator != validatorNoticed {
				writeValidator(partial, now)
			}
		case resp.StatusCode == 416 && offset > 0:
			resp.Body.Close()
			os.Remove(partial)
			continue
		default:
			resp.Body.Close()
			return fmt.Errorf("download failed: HTTP %d from %s", resp.StatusCode, red.Text(RedactURL(resp.Request.URL.String())))
		}
		f, err := obs.OpenOwnedRegular(partial, flags)
		if err != nil {
			resp.Body.Close()
			return err
		}
		done := offset
		buf := make([]byte, 1<<20)
		var werr error
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if _, err := f.Write(buf[:n]); err != nil {
					werr = err
					break
				}
				done += int64(n)
				if expected > 0 && done > expected {
					werr = fmt.Errorf("the server sent more than the expected %d bytes", expected)
					break
				}
				if err := b.check(done); err != nil {
					werr = err
					break
				}
				progress(done)
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				werr = redactErr(rerr, red)
				break
			}
		}
		resp.Body.Close()
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		var refused *RefusedError
		if errors.As(werr, &refused) {
			os.Remove(partial) // it cannot finish, and it is what is using the space
			clearValidator(partial)
		}
		if werr == nil {
			clearValidator(partial)
		}
		return werr
	}
	return errors.New("the server does not resume this download consistently")
}

// resumeRule is what a download may rely on when it resumes. With a pin, the hash decides whether
// the bytes are right, so joining them is safe. Without one, a resume needs the server's word that
// the object hasn't changed. cannotResume says so to the person, once.
type resumeRule struct {
	pinned       bool
	cannotResume func()
}

// validator is what identifies the object a partial began as, kept beside it: an ETag or a
// Last-Modified, as If-Range takes them.
type validator string

const (
	validatorNone    validator = "none"    // the server gave nothing to resume by
	validatorNoticed validator = "noticed" // and the person has been told
)

func (v validator) usable() bool { return v != "" && v != validatorNone && v != validatorNoticed }

func validatorPath(partial string) string { return partial + ".validator" }

func readValidator(partial string) validator {
	b, err := obs.ReadOwnedRegular(validatorPath(partial))
	if err != nil {
		return ""
	}
	return validator(strings.TrimSpace(string(b)))
}

func writeValidator(partial string, v validator) {
	// Without it the next attempt starts over, which costs time and is safe.
	_ = obs.WritePrivateAtomic(validatorPath(partial), []byte(v))
}

func clearValidator(partial string) {
	_ = os.Remove(validatorPath(partial)) // Without a partial, nothing reads it.
}

// validatorOf is what a response offers. A weak ETag can't be used with If-Range.
func validatorOf(resp *http.Response) validator {
	if tag := resp.Header.Get("ETag"); tag != "" && !strings.HasPrefix(tag, "W/") {
		return validator(tag)
	}
	if modified := resp.Header.Get("Last-Modified"); modified != "" {
		return validator(modified)
	}
	return validatorNone
}

func copyFile(ctx context.Context, src, dst string, b *budget, progress func(int64)) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := obs.OpenOwnedRegular(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	var done int64
	for {
		if ctx.Err() != nil {
			out.Close()
			return ctx.Err()
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, err := out.Write(buf[:n]); err != nil {
				out.Close()
				return err
			}
			done += int64(n)
			if err := b.check(done); err != nil {
				out.Close()
				os.Remove(dst)
				return err
			}
			progress(done)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return rerr
		}
	}
	return out.Close()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FetchPinned downloads a variant's missing artifacts from their pinned
// Hugging Face sources into dir, checks each against its authoritative size
// and sha256, and moves it into place. A file already there is kept when its
// sha256 matches, and otherwise fetched again, with a Progress.Note saying so.
func FetchPinned(ctx context.Context, dir string, arts []models.Artifact, hfToken string, progress func(Progress)) error {
	if progress == nil {
		progress = func(Progress) {}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	staging := filepath.Join(dir, ".staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return err
	}
	client := newClient(nil, hfToken, nil, false)
	for _, a := range arts {
		final := filepath.Join(dir, a.Filename)
		if info, err := os.Stat(final); err == nil && (a.SizeBytes == 0 || info.Size() == a.SizeBytes) {
			if a.SHA256 == "" {
				continue
			}
			progress(Progress{Phase: "verify", File: a.Filename, Done: info.Size(), Total: info.Size()})
			sum, err := fileSHA256(final)
			if err != nil {
				return err
			}
			if strings.EqualFold(sum, a.SHA256) {
				continue
			}
			// The right size isn't the right file. The source is pinned, so what comes back is
			// checked below, and a second mismatch is an error naming the file.
			progress(Progress{Phase: "download", File: a.Filename, Note: noteFetchedAgain})
			if err := os.Remove(final); err != nil {
				return err
			}
		}
		if a.Source.Kind != "huggingface" || a.Source.Repo == "" || a.Source.Revision == "" || a.SHA256 == "" {
			return fmt.Errorf("%s has no pinned source to download it from", a.Filename)
		}
		url := HFEndpoint() + "/" + a.Source.Repo + "/resolve/" + a.Source.Revision + "/" + escapePath(a.Source.Path)
		partial := filepath.Join(staging, a.Filename+".partial")
		if err := download(ctx, client, url, partial, a.SizeBytes, redactorFor(nil), newBudget(staging, a.SizeBytes), resumeRule{pinned: true}, func(n int64) {
			progress(Progress{Phase: "download", File: a.Filename, Done: n, Total: a.SizeBytes})
		}); err != nil {
			return err
		}
		sum, err := fileSHA256(partial)
		if err != nil {
			return err
		}
		if !strings.EqualFold(sum, a.SHA256) {
			os.Remove(partial)
			return &HashMismatchError{File: a.Filename, Expected: a.SHA256, Actual: sum, Trust: models.HashAuthoritative}
		}
		if err := os.Rename(partial, final); err != nil {
			return err
		}
	}
	os.Remove(staging)
	return nil
}
