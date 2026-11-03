package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg" // to read a starting picture's size
	_ "image/png"
	"math"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// The Image studio (DESIGN §22.16). A batch is N runs with consecutive seeds,
// so each picture has its own progress and Stop. The page always picks the
// seed, so every result can show it and repeat it. A look only adds words to
// the prompt and the negative prompt, and the result says which look was used.

// imageShapes maps a shape and resolution to the sizes the API accepts.
var imageShapes = map[string]map[string]string{
	"square":    {"standard": "512x512", "large": "1024x1024"},
	"portrait":  {"standard": "512x768", "large": "768x1024"},
	"landscape": {"standard": "768x512", "large": "1024x768"},
}

// imageLook is a named set of words added to a prompt.
type imageLook struct{ ID, Name, Add, Avoid string }

var imageLooks = []imageLook{
	{ID: "", Name: "Plain"},
	{ID: "photo", Name: "Photo", Add: "photograph, natural light, realistic detail", Avoid: "illustration, painting, cartoon"},
	{ID: "illustration", Name: "Illustration", Add: "digital illustration, clean shapes, soft shading", Avoid: "photograph"},
	{ID: "watercolor", Name: "Watercolor", Add: "watercolor painting, soft edges, paper texture", Avoid: "photograph, hard edges"},
	{ID: "render", Name: "3D render", Add: "3D render, clay model, smooth plastic materials, bright soft studio lighting, simple background", Avoid: "photograph, realistic, dark, film grain, flat illustration"},
	{ID: "line", Name: "Line art", Add: "black ink line drawing, pen outlines only, white background, monochrome, coloring book page", Avoid: "color, photograph, realistic, shading, gray, gradient"},
	{ID: "cinematic", Name: "Cinematic", Add: "cinematic still, dramatic lighting, shallow depth of field", Avoid: "illustration, cartoon"},
}

// imageChange is how far an edit may move from its starting picture, in plain
// words. Strength is the API's: higher keeps less of the picture.
type imageChange struct{ ID, Name, Label, Note, Strength string }

var imageChanges = []imageChange{
	{ID: "little", Name: "A little", Label: "Change a little", Note: "Keeps most of the picture", Strength: "0.35"},
	{ID: "some", Name: "Some", Label: "Change some", Note: "Keeps its layout and redraws the rest", Strength: "0.6"},
	{ID: "lot", Name: "A lot", Label: "Change a lot", Note: "Keeps only a rough idea of it", Strength: "0.85"},
}

func changeByID(id string) imageChange {
	for _, c := range imageChanges {
		if c.ID == id {
			return c
		}
	}
	return imageChanges[1]
}

func lookByID(id string) imageLook {
	for _, l := range imageLooks {
		if l.ID == id {
			return l
		}
	}
	return imageLooks[0]
}

// imageRequest is what a picture was asked for, kept with its result so it can
// be repeated: Regenerate uses the same seed, Vary a new one.
type imageRequest struct {
	Prompt, Avoid, Size, Model, Look string
	Seed                             int64
	N                                int
	Steps                            string
	Source, Change                   string // an edit: the result it starts from, and how far to move
}

// words is the prompt and the negative prompt, with the look's words added.
func (q imageRequest) words() (prompt, avoid string) {
	look := lookByID(q.Look)
	prompt, avoid = q.Prompt, q.Avoid
	if look.Add != "" {
		prompt += ", " + look.Add
	}
	if look.Avoid != "" {
		avoid = strings.Trim(avoid+", "+look.Avoid, ", ")
	}
	return prompt, avoid
}

func (q imageRequest) body() []byte {
	prompt, avoid := q.words()
	req := map[string]any{"prompt": prompt, "size": q.Size, "response_format": "b64_json", "seed": q.Seed}
	if q.N > 1 {
		req["n"] = q.N
	}
	if q.Model != "" {
		req["model"] = q.Model
	}
	if avoid != "" {
		req["negative_prompt"] = avoid
	}
	if q.Steps != "" {
		req["steps"] = json.Number(q.Steps)
	}
	b, _ := json.Marshal(req)
	return b
}

// editBody is the same request as an edit's form: the words, the settings and
// the starting picture.
func (q imageRequest) editBody(picture []byte, ctype string) ([]byte, string) {
	prompt, avoid := q.words()
	var out bytes.Buffer
	form := multipart.NewWriter(&out)
	field := func(name, value string) {
		if value != "" {
			form.WriteField(name, value)
		}
	}
	field("prompt", prompt)
	field("model", q.Model)
	field("negative_prompt", avoid)
	field("response_format", "b64_json")
	field("seed", strconv.FormatInt(q.Seed, 10))
	field("steps", q.Steps)
	field("strength", changeByID(q.Change).Strength)
	if q.N > 1 {
		field("n", strconv.Itoa(q.N))
	}
	if imageSizeOffered(q.Size) {
		field("size", q.Size)
	}
	head := textproto.MIMEHeader{}
	head.Set("Content-Disposition", `form-data; name="image"; filename="picture"`)
	head.Set("Content-Type", ctype)
	if part, err := form.CreatePart(head); err == nil {
		part.Write(picture)
	}
	form.Close()
	return out.Bytes(), form.FormDataContentType()
}

func imageSizeOffered(size string) bool {
	for _, sizes := range imageShapes {
		if sizes["standard"] == size || sizes["large"] == size {
			return true
		}
	}
	return false
}

// imageEdits says whether a picture model can start from a picture, by the same
// answer the API goes by.
func (u *UI) imageEdits(ctx context.Context, id string) bool {
	s := u.d.Catalog.ByID(id)
	if s == nil {
		return false
	}
	return runtime.CapabilitiesFor(ctx, u.d.Store, u.d.Catalog, s, filepath.Join(u.d.RootDir, "models", s.Kind)).Has("image-to-image")
}

// editRequest builds an edit's request. The new picture keeps the starting
// picture's size; msg says why there can be no edit.
func (u *UI) editRequest(r *http.Request, q *imageRequest) (body []byte, ctype, msg string) {
	source := u.results.get(u.caller(r).OwnerID(), q.Source)
	if source == nil {
		return nil, "", "The picture to edit is no longer here. Pictures stay for 30 minutes."
	}
	if u.d.Catalog == nil || !u.imageEdits(r.Context(), q.Model) {
		return nil, "", "This model can’t edit pictures. Choose one that can."
	}
	if c, _, err := image.DecodeConfig(bytes.NewReader(source.body)); err == nil {
		q.Size = fmt.Sprintf("%dx%d", c.Width, c.Height)
	}
	body, ctype = q.editBody(source.body, source.ctype)
	return body, ctype, ""
}

// randomSeed leaves room for a batch: the API takes seeds up to 2^31-1, and the
// images of a batch use seed, seed+1 and so on.
func randomSeed() int64 { return int64(rand.Int32N(math.MaxInt32 - 10)) }

// imageForm reads the Image studio's form; msg says what is wrong with it.
func imageForm(r *http.Request) (q imageRequest, msg string) {
	q = imageRequest{Prompt: r.PostFormValue("prompt"), Avoid: strings.TrimSpace(r.PostFormValue("negative_prompt")),
		Size: r.PostFormValue("size"), Model: r.PostFormValue("model"), Look: lookByID(r.PostFormValue("look")).ID}
	if sizes, ok := imageShapes[r.PostFormValue("shape")]; ok {
		q.Size = sizes["standard"]
		if s, ok := sizes[r.PostFormValue("resolution")]; ok {
			q.Size = s
		}
	}
	q.Seed = -1
	if v := strings.TrimSpace(r.PostFormValue("seed")); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 0 {
			return q, "seed must be a whole number."
		}
		q.Seed = n
	}
	if v := strings.TrimSpace(r.PostFormValue("steps")); v != "" {
		if _, err := strconv.Atoi(v); err != nil {
			return q, "steps must be a whole number."
		}
		q.Steps = v
	}
	q.N, _ = strconv.Atoi(r.PostFormValue("count"))
	if !slices.Contains([]int{1, 2, 4}, q.N) {
		q.N = 1
	}
	if q.Source = r.PostFormValue("source"); q.Source != "" {
		q.Change = changeByID(r.PostFormValue("change")).ID
	}
	return q, ""
}

// startImages starts one run for the batch and answers with its gallery group.
// False means runs are not served, and the caller should call the API directly.
func (u *UI) startImages(w http.ResponseWriter, r *http.Request, q imageRequest) bool {
	if !u.runsServed(r) {
		return false
	}
	if q.Seed < 0 {
		q.Seed = randomSeed()
	}
	target, ctype, body := "/_localbase/runs/v1/images/generations", "application/json", q.body()
	if q.Source != "" {
		var msg string
		target = "/_localbase/runs/v1/images/edits"
		if body, ctype, msg = u.editRequest(r, &q); msg != "" {
			u.labOutcome(w, r, map[string]any{"Error": msg})
			return true
		}
	}
	res := u.call(r, "POST", target, ctype, body)
	if msg := res.apiError(); msg != "" {
		u.labOutcome(w, r, map[string]any{"Error": msg})
		return true
	}
	var run labRun
	if json.Unmarshal(res.Body, &run) != nil || run.ID == "" {
		u.labOutcome(w, r, map[string]any{"Error": "LocalBase did not start the request. Try again."})
		return true
	}
	owner := u.caller(r).OwnerID()
	u.notes.put(owner, run.ID, q.Prompt)
	u.notes.setImage(owner, run.ID, q)
	v := u.labItem(r, run)
	v.Fresh = true
	w.Header().Set("HX-Retarget", "#lab-items")
	w.Header().Set("HX-Reswap", "afterbegin")
	u.fragment(w, r, "lab", "lab-item", v)
	return true
}

// labImageAgain repeats one picture of a result: Regenerate with its seed, Vary
// with a new one.
func (u *UI) labImageAgain(w http.ResponseWriter, r *http.Request) {
	note := u.notes.get(u.caller(r).OwnerID(), r.PathValue("id"))
	if note == nil || note.image == nil {
		u.labOutcome(w, r, map[string]any{"Error": "The settings for this picture are no longer available. Generate it again."})
		return
	}
	q := *note.image
	q.N = 1
	if r.PostFormValue("mode") == "same" {
		if p := note.picture(r.PostFormValue("picture")); p != nil {
			q.Seed = p.Seed
		}
	} else {
		q.Seed = randomSeed()
	}
	if !u.startImages(w, r, q) {
		u.labOutcome(w, r, map[string]any{"Error": "This LocalBase cannot repeat a picture yet."})
	}
}
