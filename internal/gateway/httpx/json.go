package httpx

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

const MaxRequestBytes = 25 << 20

func PayloadTooLarge(w http.ResponseWriter, r *http.Request) {
	// The rest of the body may still be in flight: do not reuse the connection.
	w.Header().Set("Connection", "close")
	Error(w, r, 413, "invalid_request_error", "payload_too_large", "Request body exceeds the 25 MiB limit.", nil)
}

// ReadJSON reads a bounded JSON object body, keeping unknown fields so they
// pass through to the backend unchanged.
func ReadJSON(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	if r.ContentLength > MaxRequestBytes {
		PayloadTooLarge(w, r)
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
	if err != nil {
		if r.Context().Err() != nil {
			w.WriteHeader(499)
			return nil, false
		}
		BadRequest(w, r, "Invalid JSON payload.", nil)
		return nil, false
	}
	if len(body) > MaxRequestBytes {
		PayloadTooLarge(w, r)
		return nil, false
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil || dec.More() {
		BadRequest(w, r, "Invalid JSON payload.", nil)
		return nil, false
	}
	req, ok := v.(map[string]any)
	if !ok {
		// Valid JSON that is not an object fails the request schema, as in TS.
		BadRequest(w, r, "Validation failed: : Invalid input: expected object, received "+TypeName(v), nil)
		return nil, false
	}
	return req, true
}
