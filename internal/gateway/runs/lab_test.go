package runs

import (
	"bytes"
	"mime/multipart"
	"testing"
)

// The indicator's model comes from a JSON body or a multipart field.
func TestRunRequestModel(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "a.wav")
	fw.Write([]byte("RIFF"))
	mw.WriteField("model", "whisper-base-q8_0")
	mw.Close()
	if got := requestModel(mw.FormDataContentType(), buf.Bytes()); got != "whisper-base-q8_0" {
		t.Errorf("multipart: %q", got)
	}
	if got := requestModel("application/json", []byte(`{"model":"m1","input":"x"}`)); got != "m1" {
		t.Errorf("json: %q", got)
	}
	if got := requestModel("multipart/form-data; boundary=x", []byte("junk")); got != "" {
		t.Errorf("junk: %q", got)
	}
}
