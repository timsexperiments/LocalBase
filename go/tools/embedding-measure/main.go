// Command embedding-measure answers what DESIGN §35.6 could not, against a running gateway: how
// much one input can hold, how long batches take, whether vectors arrive normalized, and whether a
// query prefix helps. tools/real-embedding-check.sh runs it, and it never starts anything itself.
//
//	embedding-measure -base http://127.0.0.1:28410 -model <id> -out results.json
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

type client struct {
	base, model string
	http        *http.Client
}

type answer struct {
	status  int
	ms      int64
	tokens  int
	vectors [][]float64
	body    string
}

func (c *client) embed(inputs []string) (*answer, error) {
	body, err := json.Marshal(map[string]any{"model": c.model, "input": inputs})
	if err != nil {
		return nil, err
	}
	began := time.Now()
	resp, err := c.http.Post(c.base+"/v1/embeddings", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	a := &answer{status: resp.StatusCode, ms: time.Since(began).Milliseconds()}
	if resp.StatusCode != 200 {
		a.body = string(raw)
		if len(a.body) > 300 {
			a.body = a.body[:300]
		}
		return a, nil
	}
	var parsed struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	a.tokens = parsed.Usage.PromptTokens
	a.vectors = make([][]float64, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(a.vectors) {
			return nil, fmt.Errorf("index %d of %d", d.Index, len(a.vectors))
		}
		a.vectors[d.Index] = d.Embedding
	}
	return a, nil
}

// wordsOf is text of n words, different each time, like a real passage to the tokenizer.
func wordsOf(n, seed int) string {
	stems := []string{"river", "market", "garden", "station", "engine", "letter", "window", "harvest", "mountain", "library", "copper", "lantern", "meadow", "bridge", "kitchen", "orchard"}
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(stems[(i*7+seed)%len(stems)])
		fmt.Fprintf(&b, "%d", (i*13+seed)%97)
	}
	return b.String()
}

type limitResult struct {
	Tried       []limitTry `json:"tried"`
	MostWords   int        `json:"mostWordsAccepted"`
	MostTokens  int        `json:"mostTokensAccepted"`
	MostChars   int        `json:"mostCharactersAccepted"`
	FirstRefuse string     `json:"firstRefusal,omitempty"`
	NeverRefuse bool       `json:"neverRefusedUpToTheLastTried"`
}

type limitTry struct {
	Words  int    `json:"words"`
	Chars  int    `json:"chars"`
	Status int    `json:"status"`
	Tokens int    `json:"tokens"`
	Ms     int64  `json:"ms"`
	Body   string `json:"body,omitempty"`
}

// measureLimit grows one input until it is refused, then narrows to within about 5%.
func measureLimit(c *client, most int) (*limitResult, error) {
	r := &limitResult{}
	try := func(n int) (bool, error) {
		text := wordsOf(n, n)
		a, err := c.embed([]string{text})
		if err != nil {
			return false, err
		}
		r.Tried = append(r.Tried, limitTry{n, len(text), a.status, a.tokens, a.ms, a.body})
		if a.status == 200 {
			if n > r.MostWords {
				r.MostWords, r.MostTokens, r.MostChars = n, a.tokens, len(text)
			}
			return true, nil
		}
		if r.FirstRefuse == "" {
			r.FirstRefuse = a.body
		}
		return false, nil
	}
	low, high := 0, 0
	for _, n := range []int{32, 64, 128, 256, 384, 512, 768, 1024, 1536, 2048, 4096, 8192, 16384} {
		if n > most {
			break
		}
		ok, err := try(n)
		if err != nil {
			return r, err
		}
		if !ok {
			high = n
			break
		}
		low = n
	}
	if high == 0 {
		r.NeverRefuse = true
		return r, nil
	}
	for high-low > low/20+1 {
		mid := (low + high) / 2
		ok, err := try(mid)
		if err != nil {
			return r, err
		}
		if ok {
			low = mid
		} else {
			high = mid
		}
	}
	return r, nil
}

type batchResult struct {
	Passages int     `json:"passages"`
	Runs     []int64 `json:"runsMs"`
	Status   int     `json:"status"`
	Tokens   int     `json:"tokens"`
	MsEach   float64 `json:"msPerPassage"`
	Body     string  `json:"body,omitempty"`
}

// measureBatches times batches of passages near the page's 1,000 characters, or less if one input
// can't hold that many.
func measureBatches(c *client, words int) ([]batchResult, error) {
	var out []batchResult
	for _, n := range []int{1, 4, 8, 16, 32, 64} {
		r := batchResult{Passages: n}
		for run := 0; run < 3; run++ {
			inputs := make([]string, n)
			for i := range inputs {
				inputs[i] = wordsOf(words, 1000*run+i+n)
			}
			a, err := c.embed(inputs)
			if err != nil {
				return out, err
			}
			r.Status, r.Tokens, r.Body = a.status, a.tokens, a.body
			if a.status != 200 {
				break
			}
			r.Runs = append(r.Runs, a.ms)
		}
		if len(r.Runs) > 0 {
			sorted := append([]int64{}, r.Runs...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
			r.MsEach = float64(sorted[len(sorted)/2]) / float64(n)
		}
		out = append(out, r)
		if r.Status != 200 {
			break
		}
	}
	return out, nil
}

func norm(v []float64) float64 {
	var sum float64
	for _, x := range v {
		sum += x * x
	}
	return math.Sqrt(sum)
}

func cosine(a, b []float64) float64 {
	var dot float64
	for i := range a {
		dot += a[i] * b[i]
	}
	if na, nb := norm(a), norm(b); na > 0 && nb > 0 {
		return dot / (na * nb)
	}
	return 0
}

type vectorResult struct {
	Dimensions    int       `json:"dimensions"`
	Norms         []float64 `json:"norms"`
	Normalized    bool      `json:"normalized"`
	SameTextAgain float64   `json:"cosineOfTheSameTextTwice"`
}

func measureVectors(c *client) (*vectorResult, error) {
	texts := []string{"a short line", "A rather longer sentence about a river that runs through an old mill town.", wordsOf(100, 3), "x", "もう少し長い日本語の文章です。"}
	a, err := c.embed(texts)
	if err != nil {
		return nil, err
	}
	if a.status != 200 {
		return nil, fmt.Errorf("vectors: %d %s", a.status, a.body)
	}
	r := &vectorResult{Normalized: true}
	for _, v := range a.vectors {
		n := norm(v)
		r.Norms = append(r.Norms, n)
		r.Dimensions = len(v)
		if math.Abs(n-1) > 0.001 {
			r.Normalized = false
		}
	}
	again, err := c.embed(texts[:1])
	if err != nil {
		return nil, err
	}
	if again.status != 200 {
		return nil, fmt.Errorf("again: %d %s", again.status, again.body)
	}
	r.SameTextAgain = cosine(a.vectors[0], again.vectors[0])
	return r, nil
}

// topics is a small corpus with a known right answer for each question. It is far too small to rank
// models. It can only show whether a prefix makes a large difference on this model.
var topics = []struct {
	passages []string
	queries  []string
}{
	{[]string{"Sourdough rises because wild yeast and bacteria ferment the flour over many hours.", "Letting bread dough rest develops gluten and gives the loaf a better crumb.", "A hot oven and steam in the first minutes give a loaf its crisp crust."},
		[]string{"why does my loaf need time to rise", "how do I get a crunchy crust on bread"}},
	{[]string{"The heart pumps blood through arteries, which carry oxygen to the body's tissues.", "Veins return blood to the heart, and valves stop it flowing backwards.", "A resting adult's heart usually beats between sixty and one hundred times a minute."},
		[]string{"what carries oxygen around the body", "what is a normal pulse"}},
	{[]string{"A mortgage is a loan secured on a house, repaid with interest over many years.", "A fixed rate keeps monthly repayments the same for an agreed period.", "Overpaying a home loan reduces the interest paid over its term."},
		[]string{"how does a home loan work", "does paying extra save money on a mortgage"}},
	{[]string{"Git records a project's history as a series of commits, each a snapshot of the files.", "A branch is a movable pointer to a commit, so work can continue in parallel.", "Merging combines the changes of two branches into one."},
		[]string{"how do I work on two features at once in version control", "what is a commit"}},
	{[]string{"Volcanoes erupt when magma rises through the crust and pressure is released.", "Lava that cools quickly forms fine-grained rock such as basalt.", "Ash from a large eruption can stay in the atmosphere and cool the climate."},
		[]string{"what makes a volcano erupt", "can eruptions change the weather"}},
	{[]string{"Tomatoes need full sun and regular watering to produce a good crop.", "Pinching out side shoots directs a plant's energy into its fruit.", "Blossom end rot is usually caused by uneven watering."},
		[]string{"why do my tomatoes have black bottoms", "how much light do tomato plants want"}},
	{[]string{"A contract needs an offer, acceptance and consideration to be binding.", "A breach of contract can entitle the other party to damages.", "A signature is not always needed, since some agreements are binding when spoken."},
		[]string{"is a verbal agreement legally valid", "what can I claim if someone breaks a deal"}},
	{[]string{"Noise cancelling headphones play an inverted copy of outside sound to reduce it.", "Bluetooth audio is compressed, and a codec decides how much quality is kept.", "An open-back headphone leaks sound but gives a wider stage."},
		[]string{"how do headphones block background noise", "does wireless audio lose quality"}},
}

type queryResult struct {
	Queries   int     `json:"queries"`
	Passages  int     `json:"passages"`
	PlainTop1 float64 `json:"plainFirstPlaceRate"`
	PlainMRR  float64 `json:"plainMeanReciprocalRank"`
	WithTop1  float64 `json:"withPrefixFirstPlaceRate"`
	WithMRR   float64 `json:"withPrefixMeanReciprocalRank"`
	Prefix    string  `json:"prefix"`
	Note      string  `json:"note"`
}

const queryPrefix = "Instruct: Given a web search query, retrieve relevant passages that answer the query\nQuery: "

// measureQueries scores each question by where the first passage of its own topic ranks.
func measureQueries(c *client) (*queryResult, error) {
	var passages []string
	var owner []int
	for t, topic := range topics {
		for _, p := range topic.passages {
			passages = append(passages, p)
			owner = append(owner, t)
		}
	}
	docs, err := c.embed(passages)
	if err != nil {
		return nil, err
	}
	if docs.status != 200 {
		return nil, fmt.Errorf("passages: %d %s", docs.status, docs.body)
	}
	score := func(prefix string) (top1, mrr float64, n int, err error) {
		for t, topic := range topics {
			for _, q := range topic.queries {
				a, err := c.embed([]string{prefix + q})
				if err != nil {
					return 0, 0, 0, err
				}
				if a.status != 200 {
					return 0, 0, 0, fmt.Errorf("query: %d %s", a.status, a.body)
				}
				order := make([]int, len(passages))
				for i := range order {
					order[i] = i
				}
				sort.SliceStable(order, func(i, j int) bool {
					return cosine(a.vectors[0], docs.vectors[order[i]]) > cosine(a.vectors[0], docs.vectors[order[j]])
				})
				for rank, i := range order {
					if owner[i] == t {
						if rank == 0 {
							top1++
						}
						mrr += 1 / float64(rank+1)
						break
					}
				}
				n++
			}
		}
		return top1 / float64(n), mrr / float64(n), n, nil
	}
	r := &queryResult{Passages: len(passages), Prefix: queryPrefix, Note: "A small corpus, which can only show a large difference. It does not rank anything."}
	var err2 error
	if r.PlainTop1, r.PlainMRR, r.Queries, err2 = score(""); err2 != nil {
		return nil, err2
	}
	if r.WithTop1, r.WithMRR, _, err2 = score(queryPrefix); err2 != nil {
		return nil, err2
	}
	return r, nil
}

func main() {
	base := flag.String("base", "", "The gateway")
	model := flag.String("model", "", "The embedding model")
	out := flag.String("out", "", "Where to write the results")
	most := flag.Int("most-words", 16384, "The longest input to try")
	flag.Parse()
	if *base == "" || *model == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: embedding-measure -base <url> -model <id> -out <file>")
		os.Exit(2)
	}
	c := &client{*base, *model, &http.Client{Timeout: 4 * time.Minute}}
	results := map[string]any{}
	save := func() {
		b, err := json.MarshalIndent(results, "", "  ")
		if err == nil {
			err = os.WriteFile(*out, b, 0o600)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	fail := func(step string, err error) {
		results["failedAt"] = step + ": " + err.Error()
		save()
		fmt.Fprintf(os.Stderr, "%s: %v\n", step, err)
		os.Exit(1)
	}

	fmt.Println("the most one input can hold")
	limit, err := measureLimit(c, *most)
	results["limit"] = limit
	if err != nil {
		fail("limit", err)
	}
	save()
	fmt.Printf("  %d words, %d tokens, %d characters; never refused: %v\n", limit.MostWords, limit.MostTokens, limit.MostChars, limit.NeverRefuse)
	if limit.MostWords == 0 {
		fail("limit", fmt.Errorf("no input was accepted: %s", limit.FirstRefuse))
	}

	fmt.Println("batches")
	words := 150 // about 1,000 characters
	if limit.MostWords < words {
		words = limit.MostWords * 3 / 4
	}
	batches, err := measureBatches(c, words)
	results["batches"] = batches
	results["batchPassageWords"] = words
	if err != nil {
		fail("batches", err)
	}
	save()
	for _, b := range batches {
		fmt.Printf("  %d passages: status %d, runs %v ms, %.0f ms each\n", b.Passages, b.Status, b.Runs, b.MsEach)
	}

	fmt.Println("vectors")
	vectors, err := measureVectors(c)
	if err != nil {
		fail("vectors", err)
	}
	results["vectors"] = vectors
	save()
	fmt.Printf("  %d dimensions, norms %v, normalized: %v, the same text twice: %.6f\n", vectors.Dimensions, vectors.Norms, vectors.Normalized, vectors.SameTextAgain)

	fmt.Println("a query prefix")
	queries, err := measureQueries(c)
	if err != nil {
		fail("queries", err)
	}
	results["queries"] = queries
	save()
	fmt.Printf("  %d questions over %d passages. Plain: first place %.2f, MRR %.3f. With the prefix: first place %.2f, MRR %.3f\n",
		queries.Queries, queries.Passages, queries.PlainTop1, queries.PlainMRR, queries.WithTop1, queries.WithMRR)
}
