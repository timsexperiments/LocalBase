package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/timsexperiments/LocalBase/go/test/lab"
)

// canary is in every prompt: it must never reach the log.
const canary = "soak-canary-7f3a"

// result is one client operation.
type result struct {
	lab.Call
	// FirstToken is a stream's time to first content.
	FirstToken time.Duration
	// Invalid says what was wrong with an answer that claimed success.
	Invalid string
	// Cut marks a stream or run that ended early.
	Cut bool
}

// class is a kind of client.
type class struct {
	name    string
	workers int
	think   time.Duration
	// llm classes share the LLM queue, and pause while a burst test owns it.
	llm bool
	// image classes pause while a burst test owns the image queue.
	image bool
	// only limits the class to phases by name (nil: every phase).
	only map[string]bool
	do   func(ctx context.Context, s *soak, w *worker) result
}

type worker struct {
	id  int
	rng *rand.Rand
}

func classes() []*class {
	return []*class{
		{name: "chat", workers: 3, think: 100 * time.Millisecond, llm: true, do: doChat},
		{name: "chat-stream", workers: 3, think: 100 * time.Millisecond, llm: true, do: doStream},
		{name: "chat-large", workers: 1, think: 5 * time.Second, llm: true, do: doChatLarge},
		{name: "embeddings", workers: 1, think: 3 * time.Second, llm: true, do: doEmbedding},
		{name: "decisions", workers: 1, think: 3 * time.Second, llm: true, do: doDecision},
		{name: "images", workers: 1, think: 300 * time.Millisecond, image: true, do: doImage},
		{name: "speech", workers: 1, think: 300 * time.Millisecond, do: doSpeech},
		{name: "transcription", workers: 1, think: 300 * time.Millisecond, do: doTranscription},
		{name: "runs", workers: 2, think: 200 * time.Millisecond, llm: true, do: doRun},
		{name: "runs-image", workers: 1, think: time.Second, image: true, do: doImageRun},
		{name: "control", workers: 1, think: 500 * time.Millisecond, do: doControl},
		{name: "unload", workers: 1, think: 1500 * time.Millisecond, only: map[string]bool{"unload": true}, do: doUnload},
	}
}

// startTraffic runs the workers until ctx ends.
func (s *soak) startTraffic(ctx context.Context) *sync.WaitGroup {
	var wg sync.WaitGroup
	id := 0
	for _, c := range classes() {
		for range c.workers {
			id++
			w := &worker{id: id, rng: rand.New(rand.NewPCG(s.o.seed, uint64(id)))} //nolint:gosec // seeded, so that a run can be repeated
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.workerLoop(ctx, c, w)
			}()
		}
	}
	return &wg
}

func (s *soak) workerLoop(ctx context.Context, c *class, w *worker) {
	// Workers start spread out, so a phase does not begin with a stampede.
	if !sleepCtx(ctx, time.Duration(w.rng.Int64N(int64(500*time.Millisecond)))) {
		return
	}
	for {
		if !s.gate.wait(ctx, c) {
			return
		}
		s.gate.enter(c)
		start := s.current()
		reqCtx, cancel := context.WithTimeout(ctx, requestLimit)
		r := c.do(reqCtx, s, w)
		cancel()
		s.gate.leave(c)
		if ctx.Err() != nil {
			return // the run ended under this request; its result means nothing
		}
		s.rec.add(c.name, w.id, r, start, s.current(), s.previous())
		if !sleepCtx(ctx, c.think/2+time.Duration(w.rng.Int64N(int64(c.think)))) {
			return
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// gate pauses traffic: all of it for a resource sample, the LLM or image classes
// for a burst test that needs their queue to itself.
type gate struct {
	all, llm, image atomic.Bool
	inflight        atomic.Int64
	llmInflight     atomic.Int64
	imageInflight   atomic.Int64
	phaseName       atomic.Pointer[string]
}

func newGate() *gate { return &gate{} }

func (g *gate) blocked(c *class) bool {
	if g.all.Load() || (c.llm && g.llm.Load()) || (c.image && g.image.Load()) {
		return true
	}
	if c.only != nil {
		name := g.phaseName.Load()
		return name == nil || !c.only[*name]
	}
	return false
}

func (g *gate) wait(ctx context.Context, c *class) bool {
	for g.blocked(c) {
		if !sleepCtx(ctx, 20*time.Millisecond) {
			return false
		}
	}
	return ctx.Err() == nil
}

func (g *gate) enter(c *class) {
	g.inflight.Add(1)
	if c.llm {
		g.llmInflight.Add(1)
	}
	if c.image {
		g.imageInflight.Add(1)
	}
}

func (g *gate) leave(c *class) {
	g.inflight.Add(-1)
	if c.llm {
		g.llmInflight.Add(-1)
	}
	if c.image {
		g.imageInflight.Add(-1)
	}
}

// settle waits for counter to reach zero.
func settle(ctx context.Context, counter *atomic.Int64, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for counter.Load() != 0 {
		if time.Now().After(deadline) || !sleepCtx(ctx, 10*time.Millisecond) {
			return false
		}
	}
	return true
}

func chatBody(model, content string) map[string]any {
	return map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": content}}}
}

func fromCall(c lab.Call) result { return result{Call: c} }

// chatContent checks a completion is from the model asked for: the fake backend
// names its model file, so an answer from the wrong runtime shows.
func chatContent(c lab.Call, model string) result {
	r := fromCall(c)
	if c.Err != nil || c.Status != 200 {
		return r
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(c.Body, &out); err != nil || len(out.Choices) == 0 {
		r.Invalid = "not a chat completion"
		return r
	}
	if file := lab.PrimaryFile(model); !strings.Contains(out.Choices[0].Message.Content, file) {
		r.Invalid = fmt.Sprintf("answered by the wrong model: %.80q, expected %s", out.Choices[0].Message.Content, file)
	}
	return r
}

func doChat(ctx context.Context, s *soak, w *worker) result {
	return chatContent(s.c.PostJSON(ctx, "/v1/chat/completions", s.g.Keys.User, chatBody(lab.ChatSmall, canary+" hello")), lab.ChatSmall)
}

func doChatLarge(ctx context.Context, s *soak, w *worker) result {
	return chatContent(s.c.PostJSON(ctx, "/v1/chat/completions", s.g.Keys.User, chatBody(lab.ChatLarge, canary+" hello")), lab.ChatLarge)
}

func words(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "w%d ", i+1)
	}
	return b.String()
}

func doStream(ctx context.Context, s *soak, w *worker) result {
	n := 2 + w.rng.IntN(4)
	body := chatBody(lab.ChatSmall, fmt.Sprintf("fake:slow %d", n))
	body["stream"] = true
	st := s.c.ChatStream(ctx, s.g.Keys.User, body, nil)
	r := result{Call: st.Call, FirstToken: st.FirstToken}
	if st.Err != nil || st.Status != 200 {
		return r
	}
	switch {
	case st.ErrorEvent != "":
		r.Cut, r.Invalid = true, "error event in the stream: "+st.ErrorEvent
		r.Code = lab.ErrorCode([]byte(st.ErrorEvent))
	case !st.Done:
		r.Cut, r.Invalid = true, fmt.Sprintf("stream ended without [DONE] after %d chunks", st.Chunks)
	case st.Content != words(n):
		r.Invalid = fmt.Sprintf("stream content %.80q, expected %d words", st.Content, n)
	case st.Finish != "stop":
		r.Invalid = "finish_reason " + st.Finish
	}
	return r
}

func doEmbedding(ctx context.Context, s *soak, w *worker) result {
	c := s.c.PostJSON(ctx, "/v1/embeddings", s.g.Keys.User, map[string]any{"model": lab.Embedding, "input": canary})
	r := fromCall(c)
	if c.Err == nil && c.Status == 200 {
		var out struct {
			Data []struct {
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		if json.Unmarshal(c.Body, &out) != nil || len(out.Data) != 1 || len(out.Data[0].Embedding) == 0 {
			r.Invalid = "not an embedding response"
		}
	}
	return r
}

func doDecision(ctx context.Context, s *soak, w *worker) result {
	c := s.c.PostJSON(ctx, "/v1/systemone", s.g.Keys.User, map[string]any{"model": lab.Decision, "state": canary,
		"questions": map[string]any{"q": map[string]any{"type": "choice", "criteria": map[string]any{"a": "first", "b": "second"}}}})
	r := fromCall(c)
	if c.Err == nil && c.Status == 200 {
		var out struct {
			Answers map[string]any `json:"answers"`
		}
		if json.Unmarshal(c.Body, &out) != nil || out.Answers["q"] == nil {
			r.Invalid = "not a decision response"
		}
	}
	return r
}

func imageBody() map[string]any {
	return map[string]any{"model": lab.Image, "prompt": canary + " a boat", "n": 1, "size": "512x512"}
}

var pngHeader = []byte("\x89PNG\r\n\x1a\n")

func checkImage(r *result, body []byte) {
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &out) != nil || len(out.Data) != 1 {
		r.Invalid = "not an image response"
		return
	}
	raw, err := base64.StdEncoding.DecodeString(out.Data[0].B64)
	if err != nil || !bytes.HasPrefix(raw, pngHeader) {
		r.Invalid = "image data is not a PNG"
	}
}

func doImage(ctx context.Context, s *soak, w *worker) result {
	c := s.c.PostJSON(ctx, "/v1/images/generations", s.g.Keys.User, imageBody())
	r := fromCall(c)
	if c.Err == nil && c.Status == 200 {
		checkImage(&r, c.Body)
	}
	return r
}

func doSpeech(ctx context.Context, s *soak, w *worker) result {
	c := s.c.PostJSON(ctx, "/v1/audio/speech", s.g.Keys.User, map[string]any{"model": lab.TTS, "input": "good morning " + canary, "voice": "default"})
	r := fromCall(c)
	if c.Err == nil && c.Status == 200 && !bytes.HasPrefix(c.Body, []byte("RIFF")) {
		r.Invalid = "speech is not a WAV"
	}
	return r
}

var transcriptionForm, transcriptionType = func() ([]byte, string) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	mw.WriteField("model", lab.STT)
	fw, _ := mw.CreateFormFile("file", "a.wav")
	_, _ = fw.Write(lab.WAV(8000)) // into a bytes.Buffer, which cannot fail
	mw.Close()
	return b.Bytes(), mw.FormDataContentType()
}()

func doTranscription(ctx context.Context, s *soak, w *worker) result {
	c := s.c.Do(ctx, "POST", "/v1/audio/transcriptions", s.g.Keys.User, transcriptionType, transcriptionForm)
	r := fromCall(c)
	if c.Err == nil && c.Status == 200 && !bytes.Contains(c.Body, []byte("harbor")) {
		r.Invalid = "not the transcript"
	}
	return r
}

// runView is the part of a run the soak reads.
type runView struct {
	ID     string         `json:"id"`
	Status string         `json:"status"`
	Reason string         `json:"reason"`
	Error  map[string]any `json:"error"`
	Output *struct {
		Content string `json:"content"`
	} `json:"output"`
}

func (v runView) code() string {
	s, _ := v.Error["code"].(string)
	return s
}

// runRefusal records a run the gateway refused or stopped as what a /v1 client
// would have been told: its error follows the same allow-list. A run has no
// response header, so Retry-After is not checked here.
func runRefusal(r *result, v runView) {
	if v.Status == "interrupted" {
		// Only critical memory or the drain may interrupt a run.
		r.Cut, r.Invalid = true, "a run interrupted: "+v.Reason
		return
	}
	r.Code, r.RetryAfter = v.code(), "run"
	r.Status = 503
	if strings.HasPrefix(r.Code, "inference_queue_") {
		r.Status = 429
	}
	r.Body = []byte(fmt.Sprintf("run %s: %v", v.Status, v.Error))
}

// doRun is a detached chat run as a page uses it: create, follow its events
// (sometimes dropping the connection and resuming from the last id), then read
// the result and dismiss it. Some runs are stopped, and a few are left behind
// for retention to deal with.
func doRun(ctx context.Context, s *soak, w *worker) result {
	key := s.g.Keys.Runs[w.id%(len(s.g.Keys.Runs)-1)] // the last owner is the quota test's
	n := 4 + w.rng.IntN(6)
	began := time.Now()
	create := s.c.PostJSON(ctx, "/_localbase/runs/v1/chat/completions", key, chatBody(lab.ChatSmall, fmt.Sprintf("fake:slow %d", n)))
	r := fromCall(create)
	if create.Err != nil || create.Status != 202 {
		return r
	}
	var v runView
	if json.Unmarshal(create.Body, &v) != nil || v.ID == "" {
		r.Invalid = "the run was not created"
		return r
	}
	mode := w.rng.IntN(10)
	stop := mode < 2
	dropAfter := 0
	if mode >= 2 && mode < 6 {
		dropAfter = 1 + w.rng.IntN(4)
	}
	problem := followEvents(ctx, s, key, v.ID, stop, dropAfter)
	r.Status, r.Dur = 200, time.Since(began)
	if problem != "" {
		r.Invalid = problem
	}
	view := s.c.Do(ctx, "GET", "/_localbase/runs/"+v.ID, key, "", nil)
	if view.Err != nil || view.Status != 200 || json.Unmarshal(view.Body, &v) != nil {
		r.Call = view
		if view.Status == 200 {
			r.Invalid = "the run could not be read"
		}
		return r
	}
	switch {
	case v.Status == "failed" || v.Status == "interrupted":
		runRefusal(&r, v)
	case stop && v.Status != "cancelled" && v.Status != "completed":
		r.Invalid = "a stopped run ended as " + v.Status
	case !stop && v.Status != "completed":
		r.Invalid = "a run ended as " + v.Status
	case !stop && (v.Output == nil || v.Output.Content != words(n)):
		got := ""
		if v.Output != nil {
			got = v.Output.Content
		}
		r.Invalid = fmt.Sprintf("a completed run has output %.80q, expected %d words", got, n)
	}
	if mode != 9 {
		del := s.c.Do(ctx, "DELETE", "/_localbase/runs/"+v.ID, key, "", nil)
		if del.Err == nil && del.Status != 200 && r.Invalid == "" {
			r.Invalid = fmt.Sprintf("dismissing a run: %d %s", del.Status, del.Code)
		}
	}
	return r
}

// followEvents reads a run's events to the end, checking the ids run on without
// gap or repeat across reconnects. It returns what was wrong, or "".
func followEvents(ctx context.Context, s *soak, key, id string, stop bool, dropAfter int) string {
	after, seen := 0, 0
	for attempt := 0; attempt < 8; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, "GET", s.g.Base+"/_localbase/runs/"+id+"/events", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		if after > 0 {
			req.Header.Set("Last-Event-ID", fmt.Sprint(after))
		}
		resp, err := s.c.HTTP.Do(req)
		if err != nil {
			return "following a run: " + err.Error()
		}
		end, gap, n := readRunEvents(resp, &after, dropAfter, stop, func() {
			s.c.PostJSON(ctx, "/_localbase/runs/"+id+"/cancel", key, "")
		})
		resp.Body.Close()
		seen += n
		if gap != "" {
			return gap
		}
		if end {
			return ""
		}
		dropAfter, stop = 0, false
	}
	return fmt.Sprintf("a run's event stream never ended (%d events)", seen)
}

func readRunEvents(resp *http.Response, after *int, dropAfter int, stop bool, cancel func()) (end bool, gap string, n int) {
	buf := make([]byte, 0, 64<<10)
	chunk := make([]byte, 32<<10)
	for {
		m, err := resp.Body.Read(chunk)
		buf = append(buf, chunk[:m]...)
		for {
			i := bytes.Index(buf, []byte("\n\n"))
			if i < 0 {
				break
			}
			block := string(buf[:i])
			buf = buf[i+2:]
			id, name := -1, ""
			for _, line := range strings.Split(block, "\n") {
				if v, ok := strings.CutPrefix(line, "id: "); ok {
					if _, err := fmt.Sscan(v, &id); err != nil {
						return false, fmt.Sprintf("an event id that is not a number: %q", v), n
					}
				}
				if v, ok := strings.CutPrefix(line, "event: "); ok {
					name = v
				}
			}
			if id < 0 {
				continue // a comment
			}
			if id != *after+1 {
				return false, fmt.Sprintf("event id %d after %d", id, *after), n
			}
			*after = id
			n++
			if name == "end" {
				return true, "", n
			}
			if dropAfter > 0 && n >= dropAfter {
				return false, "", n
			}
			if stop && n >= 2 {
				cancel()
				stop = false
			}
		}
		if err != nil {
			return false, "", n
		}
	}
}

// doImageRun is a media run: create, wait for its end, fetch the result.
func doImageRun(ctx context.Context, s *soak, w *worker) result {
	key := s.g.Keys.Runs[0]
	began := time.Now()
	create := s.c.PostJSON(ctx, "/_localbase/runs/v1/images/generations", key, imageBody())
	r := fromCall(create)
	if create.Err != nil || create.Status != 202 {
		return r
	}
	var v runView
	if json.Unmarshal(create.Body, &v) != nil || v.ID == "" {
		r.Invalid = "the run was not created"
		return r
	}
	if problem := followEvents(ctx, s, key, v.ID, false, 0); problem != "" {
		r.Status, r.Invalid = 200, problem
		return r
	}
	view := s.c.Do(ctx, "GET", "/_localbase/runs/"+v.ID, key, "", nil)
	if view.Err != nil || view.Status != 200 || json.Unmarshal(view.Body, &v) != nil {
		return fromCall(view)
	}
	r.Status, r.Dur = 200, time.Since(began)
	if v.Status == "completed" {
		res := s.c.Do(ctx, "GET", "/_localbase/runs/"+v.ID+"/result", key, "", nil)
		if res.Err != nil || res.Status != 200 {
			r.Invalid = fmt.Sprintf("fetching a completed run's result: %d %v", res.Status, res.Err)
		} else {
			checkImage(&r, res.Body)
		}
	} else {
		runRefusal(&r, v)
	}
	s.c.Do(ctx, "DELETE", "/_localbase/runs/"+v.ID, key, "", nil)
	return r
}

// doControl is what stays up through everything: health and the model lists.
func doControl(ctx context.Context, s *soak, w *worker) result {
	var last lab.Call
	for _, path := range []string{"/health", "/health/ready", "/v1/models", "/_localbase/models"} {
		c := s.c.Do(ctx, "GET", path, s.g.Keys.User, "", nil)
		if c.Err != nil || c.Status != 200 {
			c.Body = []byte(path + ": " + string(c.Body))
			return fromCall(c)
		}
		last = c
	}
	return fromCall(last)
}

var unloadTargets = []string{lab.ChatSmall, lab.ChatLarge, lab.Embedding, lab.Decision, lab.Image, lab.TTS, lab.STT}

func doUnload(ctx context.Context, s *soak, w *worker) result {
	id := unloadTargets[w.rng.IntN(len(unloadTargets))]
	query := ""
	if w.rng.IntN(2) == 0 {
		query = "?wait=false"
	}
	return fromCall(s.c.Do(ctx, "POST", "/_localbase/models/"+id+"/unload"+query, s.g.Keys.Admin, "", nil))
}
