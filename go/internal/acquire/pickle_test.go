package acquire

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The usual way a pickle arrives: a name nothing refuses, and bytes that are only known once they're
// here. It stops before anything can load it, with nothing left behind.
func TestAnImportedPickleIsNotPublished(t *testing.T) {
	pickle := []byte("\x80\x02}q\x00(X\x01\x00\x00\x00aq\x01K\x01u.padding to look like weights")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRange(w, r, pickle)
	}))
	defer srv.Close()
	root := t.TempDir()
	ctx := context.Background()
	p, err := Resolve(ctx, root, Request{Source: srv.URL + "/pytorch_model.bin", AllowHTTP: true, AllowPrivate: true, Confirmed: true, Task: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Import(ctx, root, p, nil)
	if err == nil || !strings.Contains(err.Error(), "pickle") {
		t.Errorf("the error: %v", err)
	}
	filepath.WalkDir(root, func(path string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() && strings.HasPrefix(d.Name(), "pytorch_model.bin") {
			t.Errorf("left behind: %s", d.Name())
		}
		return nil
	})
}
