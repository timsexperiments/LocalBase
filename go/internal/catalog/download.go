package catalog

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DownloadURL is where an artifact downloads from.
func DownloadURL(s *Spec, a Artifact) string {
	repo, rev := s.Source, s.RepositoryRevision
	if len(a.Source) > 0 {
		var src struct{ RepositoryURL, Revision string }
		if json.Unmarshal(a.Source, &src) == nil && src.RepositoryURL != "" {
			repo, rev = src.RepositoryURL, src.Revision
		}
	}
	base := strings.TrimSuffix(repo, "/")
	path := strings.TrimLeft(a.SourcePath, "/")
	if u, err := url.Parse(base); err == nil && u.Host == "github.com" {
		parts := strings.Split(path, "/")
		for i, p := range parts {
			parts[i] = url.PathEscape(p)
		}
		return "https://raw.githubusercontent.com" + u.Path + "/" + rev + "/" + strings.Join(parts, "/")
	}
	return base + "/resolve/" + rev + "/" + path
}

// RequiredBytes is the disk an install still needs, crediting complete files
// and partial downloads.
func RequiredBytes(s *Spec, dir string) int64 {
	var total int64
	for _, a := range s.Artifacts {
		final := filepath.Join(dir, a.Filename)
		if st, err := os.Stat(final); err == nil && st.Size() == a.ExpectedSizeBytes {
			continue
		}
		remaining := a.ExpectedSizeBytes
		if st, err := os.Stat(final + ".partial"); err == nil {
			remaining = max(0, remaining-st.Size())
		}
		if st, err := os.Stat(final); err == nil && st.Size() < a.ExpectedSizeBytes {
			remaining = min(remaining, a.ExpectedSizeBytes-st.Size())
		}
		total += remaining
	}
	return total
}
