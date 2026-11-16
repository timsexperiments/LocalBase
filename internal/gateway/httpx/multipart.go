package httpx

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
)

// FormPart is one part of a form ReadMultipart read: a value, or a file with its name and type.
type FormPart struct {
	Value    string
	File     []byte
	Filename string
	CType    string // from the filename extension, as Bun's File.type; the part header is ignored
	IsFile   bool
}

// ReadMultipart parses a bounded multipart body: values per field, in order.
// include[] and timestamp_granularities[] are repeated fields.
func ReadMultipart(r *http.Request, body []byte) (map[string][]FormPart, error) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return nil, errors.New("not multipart")
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	out := map[string][]FormPart{}
	for parts := 0; ; parts++ {
		p, err := mr.NextPart()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if parts == MaxFormParts {
			return nil, errors.New("too many parts")
		}
		data, err := io.ReadAll(p)
		if err != nil {
			return nil, err
		}
		name := p.FormName()
		part := FormPart{Value: string(data)}
		// A filename parameter makes a file part, even when empty (as in Bun).
		if _, dp, err := mime.ParseMediaType(p.Header.Get("Content-Disposition")); err == nil {
			if fn, ok := dp["filename"]; ok {
				part = FormPart{File: data, Filename: fn, CType: fileTypeOf(fn), IsFile: true}
			}
		}
		out[name] = append(out[name], part)
	}
}
