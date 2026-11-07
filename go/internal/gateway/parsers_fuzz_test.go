package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/mediacheck"
)

// These feed what a caller or a backend can send to what reads it first. Plain `go test` runs the
// seeds, and tools/fuzz.sh fuzzes on demand. Each must never panic or overflow the stack.

func FuzzValidators(f *testing.F) {
	for _, seed := range []string{
		`{}`, `null`, `[]`, `1`, `{"model":""}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}]}`,
		`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"n","arguments":"{}"}}]}]}`,
		`{"model":"m","messages":[1,null,[],{}],"stop":[1],"logit_bias":{"a":"b"},"tools":[1,{"type":"function"}],"tool_choice":{"type":"function","function":{"name":"n"}}}`,
		`{"model":"m","messages":[],"response_format":{"type":"json_schema","json_schema":{"name":"a","schema":{"type":"object"}}}}`,
		`{"model":"m","questions":{"q":{"type":"score","criteria":[1,2]},"r":{"type":"choice","criteria":{"":1}},"s":{"type":"noul","criteria":{"x":1}}},"files":[1]}`,
		`{"model":"m","input":[[1],["a"],1]}`,
		`{"model":"m","prompt":"x","n":1,"size":"1x1"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var v any
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if dec.Decode(&v) != nil {
			return
		}
		validateVideoCreate(v)
		req, ok := v.(map[string]any)
		if !ok {
			return
		}
		for _, c := range []*checker{validateChat(req), validateEmbeddings(req), validateDecision(req), validateImageGeneration(req)} {
			if !c.ok() {
				c.reply(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil))
			}
		}
	})
}

func FuzzValidateSpeech(f *testing.F) {
	for _, seed := range []string{`{}`, `null`, `[]`, `{"model":"m","input":"x","voice":"v","speed":1,"instructions":"y","z":1}`, `{"input":1}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) { validateSpeech(data) })
}

func FuzzMultipart(f *testing.F) {
	const b = "BOUNDARY"
	part := func(name, filename, body string) string {
		if filename != "" {
			return fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=%q; filename=%q\r\n\r\n%s\r\n", b, name, filename, body)
		}
		return fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=%q\r\n\r\n%s\r\n", b, name, body)
	}
	f.Add(b, []byte{})
	f.Add(b, []byte(part("model", "", "m")+part("file", "a.wav", "x")+"--"+b+"--\r\n"))
	f.Add(b, []byte(part("image[]", "a.png", "x")+part("image[]", "", "y")))
	f.Add("", []byte("--\r\n"))
	f.Add(`a"b`, []byte("--a\"b\r\n\r\n"))
	f.Fuzz(func(t *testing.T, boundary string, body []byte) {
		ctype := "multipart/form-data; boundary=" + boundary
		r := httptest.NewRequest("POST", "/", bytes.NewReader(body))
		r.Header.Set("Content-Type", ctype)
		_, _ = readMultipart(r, body)
		_ = requestModel(ctype, body)
	})
}

func FuzzImageChecks(f *testing.F) {
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYPgPAAEDAQAIicLsAAAAAElFTkSuQmCC")
	f.Add(png)
	f.Add([]byte{})
	f.Add([]byte("\x89PNG\r\n\x1a\n"))
	f.Add([]byte{0xff, 0xd8, 0xff, 0xc0, 0x00, 0x11, 0x08, 0x00, 0x01, 0x00, 0x01, 0x03, 0x01, 0x11, 0x00, 0x02, 0x11, 0x00, 0x03, 0x11, 0x00, 0xff, 0xd9})
	f.Add([]byte{0xff, 0xd8})
	f.Fuzz(func(t *testing.T, data []byte) {
		mediacheck.ValidatePNG(data)
		mediacheck.ValidateJPEG(data)
		decodeCanonicalBase64(string(data))
	})
}

// A backend is trusted to start and not trusted to be well-formed: its stream goes through this.
func FuzzRelayStream(f *testing.F) {
	for _, seed := range []string{
		"", "data: [DONE]\n\n", "data: {}\n\n", "data: not json\n\n", ": comment\n\n", "event: x\ndata: {\"a\":1}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"a\",\"reasoning_content\":\"b\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1}}\n\ndata: [DONE]\n\n",
		"data: {\"choices\":[1,null,{\"delta\":1}]}\n\n", "data: {\"error\":{\"message\":\"x\"}}\n\n", "data: {\"model\":1,\"choices\":{}}\n\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		w := httptest.NewRecorder()
		relaySSE(context.Background(), w, bytes.NewReader(data), "m", &inference{}, func() bool { return false })
	})
}
