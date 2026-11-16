package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// hubAnsweringID answers every model lookup with information that names the repository id.
func hubAnsweringID(t *testing.T, id string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": id, "sha": "0123456789abcdef0123456789abcdef01234567"})
	}))
	t.Cleanup(srv.Close)
	return &Client{Root: t.TempDir(), Endpoint: srv.URL}
}

// The id the Hub answers with is a repository id like any other. One that isn't is refused, as an answer
// LocalBase doesn't understand, before anything is built from it or put in an address.
func TestAModelIDThatIsntARepositoryIsRefused(t *testing.T) {
	for _, id := range []string{"noslash", "org/repo/extra", "org/..", "../repo", "org/repo?x=1", "org/repo#x", "/repo", "org/"} {
		t.Run(id, func(t *testing.T) {
			info, err := hubAnsweringID(t, id).ModelInfo(context.Background(), "org/repo")
			var refused *Error
			if !errors.As(err, &refused) || refused.Message != "Hugging Face returned model information for org/repo that LocalBase does not understand." {
				t.Errorf("%+v, %v", info, err)
			}
		})
	}
}

// The Hub answers a renamed repository with its new name, so a valid id that differs from the one asked for
// is kept.
func TestARenamedRepositoryKeepsItsNewName(t *testing.T) {
	info, err := hubAnsweringID(t, "org/new-name").ModelInfo(context.Background(), "org/old-name")
	if err != nil || info.ID != "org/new-name" {
		t.Errorf("%+v, %v", info, err)
	}
}
