package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
)

// What a refusal costs follows from the size of the request, whatever is wrong with it. A request
// at the body limit allocates at most this: 2.6 GiB when measured, and 3.6 GiB under the race
// detector, which turns sync.Pool off.
const refusalAtTheBodyLimit = 4 << 30

// An answer is at most this. It's about 14 KiB for the requests at the body limit below.
const largestAnswer = 1 << 20

func refusalCost(t *testing.T, url, body string) (status int, answer string, allocated uint64) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	return resp.StatusCode, string(b), after.TotalAlloc - before.TotalAlloc
}

func refusalMessage(t *testing.T, answer string) string {
	t.Helper()
	var out struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(answer), &out); err != nil {
		t.Fatalf("%v: %.300s", err, answer)
	}
	return out.Error.Message
}

func repeated(item string, n int) string {
	return strings.TrimSuffix(strings.Repeat(item+",", n), ",")
}

func chatRefusal(messages string) string {
	return `{"model":"` + ChatModel + `","messages":[` + messages + `]}`
}

func scoreRefusal(id string, n int) string {
	return `{"model":"` + decisionModel + `","questions":{"` + id + `":{"type":"score","criteria":[` + repeated("1", n) + `]}}}`
}

func choiceRefusal(key string, n int) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"%s%d":1`, key, i)
	}
	return `{"model":"` + decisionModel + `","questions":{"q":{"type":"choice","criteria":{` + b.String() + `}}}}`
}

func speechKeys(n int) string {
	var b strings.Builder
	b.WriteString(`{"model":"x","input":"x","voice":"x"`)
	for i := range n {
		fmt.Fprintf(&b, `,"unknown%04d":1`, i)
	}
	b.WriteByte('}')
	return b.String()
}

func unknownKeys(n int) string {
	var b strings.Builder
	b.WriteString(`{"model":"` + decisionModel + `","questions":{"q":{"type":"noul"}}`)
	for i := range n {
		fmt.Fprintf(&b, `,"unknown%04d":1`, i)
	}
	b.WriteByte('}')
	return b.String()
}

// Small requests, so that a limit that isn't working costs little here. Each failure stops the
// run before TestARefusalAtTheBodyLimit could take the machine's memory.
func smallRefusals() map[string]struct{ path, body string } {
	long := strings.Repeat("a", 16<<10)
	return map[string]struct{ path, body string }{
		"messages":            {"/v1/chat/completions", chatRefusal(repeated("1", 10_000))},
		"content parts":       {"/v1/chat/completions", chatRefusal(`{"role":"user","content":[` + repeated("1", 10_000) + `]}`)},
		"stop":                {"/v1/chat/completions", `{"model":"` + ChatModel + `","messages":[],"stop":[` + repeated("1", 10_000) + `]}`},
		"tools":               {"/v1/chat/completions", `{"model":"` + ChatModel + `","messages":[],"tools":[` + repeated("1", 10_000) + `]}`},
		"logit_bias":          {"/v1/chat/completions", `{"model":"` + ChatModel + `","messages":[],"logit_bias":{"` + long + `":"x"}}`},
		"unknown keys":        {"/v1/systemone", unknownKeys(2_000)},
		"unknown speech keys": {"/v1/audio/speech", speechKeys(2_000)},
		"long question id":    {"/v1/systemone", scoreRefusal(long, 2_000)},
		"long option keys":    {"/v1/systemone", choiceRefusal(strings.Repeat("o", 1<<10), 200)},
		"many files":          {"/v1/systemone", `{"model":"` + decisionModel + `","questions":{},"files":[` + repeated("1", 10_000) + `]}`},
		"many questions":      {"/v1/systemone", `{"model":"` + decisionModel + `","questions":{` + questions(5_000) + `}}`},
		"control characters":  {"/v1/systemone", scoreRefusal(strings.Repeat(`\u0001`, 5<<10), 2_000)},
		"embeddings input":    {"/v1/embeddings", `{"model":"` + embeddingModel + `","input":[` + repeated("null", 10_000) + `]}`},
	}
}

func questions(n int) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"q%d":1`, i)
	}
	return b.String()
}

func checkSmallRefusals(t *testing.T, ts string) {
	t.Helper()
	for name, c := range smallRefusals() {
		status, answer, allocated := refusalCost(t, ts+c.path, c.body)
		if status != 400 {
			t.Errorf("%s: %d %.200s", name, status, answer)
		}
		if len(answer) > 256<<10 {
			t.Errorf("%s: a %d byte answer to a %d byte request", name, len(answer), len(c.body))
		}
		if bound := uint64(32<<20 + 64*len(c.body)); allocated > bound {
			t.Errorf("%s: %d MiB allocated for a %d byte request, over %d MiB", name, allocated>>20, len(c.body), bound>>20)
		}
	}
}

func TestARefusalFollowsTheSizeOfTheRequest(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	checkSmallRefusals(t, ts.URL)
}

// The largest request the gateway reads, in the shapes that cost the most to read and to refuse.
func TestARefusalAtTheBodyLimit(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	checkSmallRefusals(t, ts.URL)
	if t.Failed() {
		t.Fatal("the limits aren't working, so the larger requests would cost too much to try")
	}
	id := strings.Repeat("a", httpx.MaxRequestBytes/2)
	for name, c := range map[string]struct{ path, body string }{
		"numbers":     {"/v1/chat/completions", fill(`{"model":"`+ChatModel+`","messages":[`, "1", `]}`)},
		"question id": {"/v1/systemone", scoreRefusal(id, (httpx.MaxRequestBytes/2-256)/2)},
	} {
		if len(c.body) > httpx.MaxRequestBytes {
			t.Fatalf("%s: %d bytes is over the limit", name, len(c.body))
		}
		status, answer, allocated := refusalCost(t, ts.URL+c.path, c.body)
		t.Logf("%s: %d bytes, answer %d bytes, %d MiB allocated", name, len(c.body), len(answer), allocated>>20)
		if status != 400 {
			t.Errorf("%s: %d %.200s", name, status, answer)
		}
		if len(answer) > 256<<10 {
			t.Errorf("%s: a %d byte answer", name, len(answer))
		}
		if allocated > refusalAtTheBodyLimit {
			t.Errorf("%s: %d MiB allocated, over %d MiB", name, allocated>>20, refusalAtTheBodyLimit>>20)
		}
	}
}

// fill makes a list of item up to the body limit.
func fill(head, item, tail string) string {
	n := (httpx.MaxRequestBytes - len(head) - len(tail)) / (len(item) + 1)
	return head + repeated(item, n) + tail
}

// The most an answer can hold: every problem a list of the longest keys, under the longest key,
// all of them escaped.
func TestTheLargestAnswer(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	long := strings.Repeat(`\u0001`, maxPathKey)
	var keys, questions strings.Builder
	for i := range maxListedKeys + 1 {
		fmt.Fprintf(&keys, `,"%s%02d":1`, long, i)
	}
	for i := range maxValidationIssues + 1 {
		if i > 0 {
			questions.WriteByte(',')
		}
		fmt.Fprintf(&questions, `"%s%03d":{"type":"noul"%s}`, long, i, keys.String())
	}
	status, answer, _ := refusalCost(t, ts.URL+"/v1/systemone", `{"model":"`+decisionModel+`","questions":{`+questions.String()+`}}`)
	t.Logf("%d bytes", len(answer))
	if status != 400 || !strings.Contains(answer, "and more") {
		t.Errorf("%d %.200s", status, answer)
	}
	if len(answer) > largestAnswer || len(answer) < largestAnswer/2 {
		t.Errorf("a %d byte answer: this should be the most one can hold, and be under %d", len(answer), largestAnswer)
	}
}

// Nothing is built for problems past the limit, on the decision route, where no later part of the
// request can change the answer.
func TestADecisionStopsAtTheLimit(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	const many = 400_000
	id := strings.Repeat("a", 64)
	for name, body := range map[string]func(int) string{
		"score":  func(n int) string { return scoreRefusal(id, n) },
		"choice": func(n int) string { return choiceRefusal("o", n) },
		"files": func(n int) string {
			return `{"model":"` + decisionModel + `","questions":{},"files":[` + repeated("1", n) + `]}`
		},
		"question": func(n int) string { return `{"model":"` + decisionModel + `","questions":{` + questions(n) + `}}` },
	} {
		decodeOnly := func(s string) uint64 {
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			var v any
			dec := json.NewDecoder(strings.NewReader(s))
			dec.UseNumber()
			if err := dec.Decode(&v); err != nil {
				t.Fatal(err)
			}
			runtime.ReadMemStats(&after)
			return after.TotalAlloc - before.TotalAlloc
		}
		large := body(many)
		status, answer, allocated := refusalCost(t, ts.URL+"/v1/systemone", large)
		if status != 400 || !strings.HasSuffix(refusalMessage(t, answer), ", and more") {
			t.Errorf("%s: %d %.200s", name, status, answer)
		}
		read := decodeOnly(large)
		// Reading it and answering take this. Text for every element past the limit takes several
		// times that, under the race detector or not.
		if bound := read + 7*uint64(len(large)) + 2<<20; allocated > bound {
			t.Errorf("%s: %d MiB allocated, over %d MiB, where reading it takes %d MiB", name, allocated>>20, bound>>20, read>>20)
		}
	}
}

// A request with a few problems reads as it always did.
func TestAFewProblemsReadAsBefore(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	for _, c := range []struct{ path, body, want string }{
		{"/v1/chat/completions", chatRefusal(`1,2`),
			"Validation failed: messages.0: Invalid input: expected object, received number, messages.1: Invalid input: expected object, received number"},
		{"/v1/systemone", scoreRefusal("q1", 1),
			"Validation failed: questions.q1.criteria.0: Invalid input: expected string, received number, questions.q1.criteria: Too small: expected array to have >=2 items"},
		{"/v1/systemone", `{"model":"` + decisionModel + `","questions":{"q":{"type":"noul"}},"b":1,"a":2}`,
			`Validation failed: : Unrecognized keys: "a", "b"`},
	} {
		_, answer, _ := refusalCost(t, ts.URL+c.path, c.body)
		if got := refusalMessage(t, answer); got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}
}

func TestAnAnswerListsUpToTheLimitAndSaysThereIsMore(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	listed := func(n int) string {
		t.Helper()
		_, answer, _ := refusalCost(t, ts.URL+"/v1/chat/completions", chatRefusal(repeated("1", n)))
		return refusalMessage(t, answer)
	}
	all := listed(maxValidationIssues)
	if strings.Contains(all, "and more") || strings.Count(all, "messages.") != maxValidationIssues {
		t.Errorf("exactly at the limit: %d listed: ...%.100s", strings.Count(all, "messages."), all[len(all)-100:])
	}
	over := listed(maxValidationIssues + 1)
	if !strings.HasSuffix(over, ", and more") || strings.Count(over, "messages.") != maxValidationIssues {
		t.Errorf("past the limit: %d listed: ...%.100s", strings.Count(over, "messages."), over[len(over)-100:])
	}
	if !strings.HasPrefix(over, "Validation failed: messages.0: ") {
		t.Errorf("not the first ones: %.100s", over)
	}
}

func TestAKeyIsShortenedInAnAnswer(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	message := func(id string) string {
		t.Helper()
		_, answer, _ := refusalCost(t, ts.URL+"/v1/systemone", scoreRefusal(id, 1))
		return refusalMessage(t, answer)
	}
	exact := strings.Repeat("a", maxPathKey)
	if got := message(exact); !strings.Contains(got, "questions."+exact+".criteria") {
		t.Errorf("a key at the limit was changed: %.200s", got)
	}
	got := message(exact + "b")
	if !strings.Contains(got, "questions."+exact+"....criteria") || strings.Contains(got, exact+"b") {
		t.Errorf("a key past the limit: %.200s", got)
	}
	// Cut between characters, not inside one.
	wide := "a" + strings.Repeat("é", maxPathKey) // the limit falls inside a two-byte character
	got = message(wide)
	if !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) || !strings.Contains(got, "a"+strings.Repeat("é", (maxPathKey-2)/2)+"....criteria") {
		t.Errorf("a wide key past the limit: %.200s", got)
	}
}

func TestUnknownKeysAreListedUpToTheLimit(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	message := func(n int) string {
		t.Helper()
		_, answer, _ := refusalCost(t, ts.URL+"/v1/systemone", unknownKeys(n))
		return refusalMessage(t, answer)
	}
	all := message(maxListedKeys)
	if strings.Contains(all, "and more") || strings.Count(all, `"unknown`) != maxListedKeys {
		t.Errorf("exactly at the limit: %s", all)
	}
	over := message(maxListedKeys + 1)
	if !strings.HasSuffix(over, ", and more") || strings.Count(over, `"unknown`) != maxListedKeys || !strings.Contains(over, `"unknown0000"`) {
		t.Errorf("past the limit: %s", over)
	}
	_, answer, _ := refusalCost(t, ts.URL+"/v1/systemone",
		`{"model":"`+decisionModel+`","questions":{"q":{"type":"noul"}},"`+strings.Repeat("k", maxPathKey+1)+`":1}`)
	if got := refusalMessage(t, answer); strings.Contains(got, strings.Repeat("k", maxPathKey+1)) || !strings.Contains(got, strings.Repeat("k", maxPathKey)+`..."`) {
		t.Errorf("a long unknown key: %.200s", got)
	}
}

func TestUnknownSpeechKeysAreListedUpToTheLimit(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	message := func(body string) string {
		t.Helper()
		status, answer, _ := refusalCost(t, ts.URL+"/v1/audio/speech", body)
		if status != 400 {
			t.Fatalf("%d %.200s", status, answer)
		}
		return refusalMessage(t, answer)
	}
	all := message(speechKeys(maxListedKeys))
	if strings.Contains(all, "and more") || strings.Count(all, "Unrecognized key") != maxListedKeys {
		t.Errorf("exactly at the limit: %s", all)
	}
	over := message(speechKeys(maxListedKeys + 1))
	if !strings.HasSuffix(over, ", and more") || strings.Count(over, "Unrecognized key") != maxListedKeys {
		t.Errorf("past the limit: %s", over)
	}
	long := strings.Repeat("k", maxPathKey+1)
	got := message(`{"model":"x","input":"x","voice":"x","` + long + `":1}`)
	if strings.Contains(got, long) || !strings.Contains(got, strings.Repeat("k", maxPathKey)+`..."`) {
		t.Errorf("a long key: %.200s", got)
	}
}

// Past the limit, content that names an address is still what the answer is about.
func TestAddressContentStillWinsPastTheLimit(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	address := `{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://127.0.0.1:1/x.png"}}]}`
	status, answer, _ := refusalCost(t, ts.URL+"/v1/chat/completions", chatRefusal(repeated("1", maxValidationIssues*2)+","+address))
	if status != 400 || !strings.Contains(answer, `"remote_media_not_allowed"`) {
		t.Errorf("%d %.200s", status, answer)
	}
}

// A request with too many problems is as refused as one with a few.
func TestTooManyProblemsIsStillARefusal(t *testing.T) {
	c := &checker{}
	for i := range maxValidationIssues * 2 {
		c.fail("p", fmt.Sprint(i))
	}
	if c.ok() || len(c.issues) != maxValidationIssues {
		t.Errorf("ok %v, %d kept", c.ok(), len(c.issues))
	}
}

// Clients can see these, and the documents state them.
func TestTheLimitsAreTheStatedOnes(t *testing.T) {
	if maxValidationIssues != 100 || maxPathKey != 64 || maxListedKeys != 20 {
		t.Errorf("%d issues, %d bytes of a key, %d unrecognized keys", maxValidationIssues, maxPathKey, maxListedKeys)
	}
}
