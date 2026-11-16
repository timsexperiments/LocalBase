package catalog

import "testing"

func TestDownloadURL(t *testing.T) {
	a := Artifact{SourcePath: "/dir/my file.bin"}
	for source, want := range map[string]string{
		"https://huggingface.co/org/repo/": "https://huggingface.co/org/repo/resolve/rev/dir/my file.bin",
		"https://github.com/org/repo":      "https://raw.githubusercontent.com/org/repo/rev/dir/my%20file.bin",
	} {
		if got := DownloadURL(&Spec{Source: source, RepositoryRevision: "rev"}, a); got != want {
			t.Errorf("%s: %s", source, got)
		}
	}
	a.Source = []byte(`{"repositoryUrl":"https://huggingface.co/other/repo","revision":"pinned"}`)
	if got := DownloadURL(&Spec{Source: "https://huggingface.co/org/repo", RepositoryRevision: "rev"}, a); got != "https://huggingface.co/other/repo/resolve/pinned/dir/my file.bin" {
		t.Errorf("an artifact's own source: %s", got)
	}
}
