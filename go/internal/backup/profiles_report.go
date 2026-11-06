package backup

import (
	"sort"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
)

// DownloadCredential is a download profile a backup holds: where its headers will be sent. It never holds a
// value. The huggingface profile also sends the Hugging Face token to its origins.
type DownloadCredential struct {
	ID      string   `json:"id"`
	Headers []string `json:"headers,omitempty"`
	Origins []string `json:"origins"`
}

// downloadCredentials checks each profile as auth-profiles add does, and says where each will be sent. A file
// written by hand can hold what that command would refuse, and the downloader trusts what it reads.
func downloadCredentials(ps *acquire.Profiles) ([]DownloadCredential, error) {
	var out []DownloadCredential
	for i := range ps.Profiles {
		p := ps.Profiles[i]
		if err := p.Validate(); err != nil {
			return nil, refuse("The download credentials in this backup are not valid: %v", err)
		}
		d := DownloadCredential{ID: p.ID, Origins: append([]string{}, p.Origins...)}
		for name := range p.Headers {
			d.Headers = append(d.Headers, name)
		}
		sort.Strings(d.Headers)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
