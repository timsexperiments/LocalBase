package backup

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The report says where each download credential will be sent, by id, with header names and never their values.
func TestTheReportSaysWhereEachCredentialIsSent(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	write(t, filepath.Join(r.Dir, "credentials", "auth-profiles.json"), `{"profiles":[
		{"id":"vendor","origins":["https://vendor.example.com"],"headers":{"X-Api-Key":"`+secretHeader+`"}},
		{"id":"huggingface","origins":["https://elsewhere.example"],"headers":{}}]}`)
	rep := stageFile(t, create(t, r, false).Output)
	var ids []string
	for _, d := range rep.Downloads {
		ids = append(ids, d.ID)
	}
	if !slices.Equal(ids, []string{"huggingface", "vendor"}) {
		t.Fatalf("credentials, by id: %+v", rep.Downloads)
	}
	if d := rep.Downloads[0]; !slices.Equal(d.Origins, []string{"https://elsewhere.example"}) || len(d.Headers) != 0 {
		t.Errorf("the Hugging Face profile: %+v", d)
	}
	if d := rep.Downloads[1]; !slices.Equal(d.Origins, []string{"https://vendor.example.com"}) || !slices.Equal(d.Headers, []string{"X-Api-Key"}) {
		t.Errorf("the vendor profile: %+v", d)
	}
	whole, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(whole), secretHeader) {
		t.Error("a header's value is in the report")
	}
}

// What auth-profiles add refuses, a restore refuses too.
func TestDownloadCredentialsAreCheckedAsTheCommandChecksThem(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	res := create(t, r, false)
	for name, profiles := range map[string]string{
		"an origin with a path":    `{"profiles":[{"id":"vendor","origins":["https://vendor.example.com/x"],"headers":{"X-Api-Key":"v"}}]}`,
		"a header it controls":     `{"profiles":[{"id":"vendor","origins":["https://vendor.example.com"],"headers":{"Host":"v"}}]}`,
		"an escape in an origin":   `{"profiles":[{"id":"vendor","origins":["https://vendor.example.com\u001b[2J"],"headers":{"X-Api-Key":"v"}}]}`,
		"an id it wouldn't take":   `{"profiles":[{"id":"Vendor Profile","origins":["https://vendor.example.com"],"headers":{"X-Api-Key":"v"}}]}`,
		"a profile with no origin": `{"profiles":[{"id":"vendor","origins":[],"headers":{"X-Api-Key":"v"}}]}`,
		"a value on two lines":     `{"profiles":[{"id":"vendor","origins":["https://vendor.example.com"],"headers":{"X-Api-Key":"a\nb"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			entries := read(t, res.Output)
			for i := range entries {
				if entries[i].header.Name == filesPrefix+"credentials/auth-profiles.json" {
					entries[i].body = []byte(profiles)
				}
			}
			reseal(t, entries)
			file := filepath.Join(dir(t), "crafted"+Extension)
			pack(t, file, entries)
			_, err := Stage(t.Context(), file, filepath.Join(dir(t), "new"), false)
			if err == nil || !strings.HasPrefix(err.Error(), "The download credentials in this backup are not valid: ") {
				t.Fatalf("restore: %v", err)
			}
		})
	}
}
