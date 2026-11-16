package models

import (
	"strings"
	"testing"
)

// A definition is read on its own or in either wrapping, and what isn't JSON says so in the JSON's
// words.
func TestParseDefinition(t *testing.T) {
	for name, in := range map[string]string{
		"bare":       `{"id":"a.model","name":"A"}`,
		"model":      `{"model":{"id":"a.model","name":"A"}}`,
		"data.model": `{"data":{"model":{"id":"a.model","name":"A"}},"ok":true}`,
		"data wins":  `{"model":{"id":"other","name":"B"},"data":{"model":{"id":"a.model","name":"A"}}}`,
	} {
		m, err := ParseDefinition([]byte(in))
		if err != nil || m.ID != "a.model" || m.Name != "A" {
			t.Errorf("%s: %+v, %v", name, m, err)
		}
	}
	if _, err := ParseDefinition([]byte("{not json")); err == nil || !strings.Contains(err.Error(), "invalid character 'n'") {
		t.Errorf("not JSON: %v", err)
	}
	// A wrapping with nothing in it is read as a bare model with no id, for Validate to refuse.
	if m, err := ParseDefinition([]byte(`{"data":{}}`)); err != nil || m.ID != "" {
		t.Errorf("empty wrapping: %+v, %v", m, err)
	}
}
