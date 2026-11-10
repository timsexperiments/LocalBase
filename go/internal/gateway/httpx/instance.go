package httpx

import (
	"crypto/subtle"
	"net/http"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// InstanceTokenMatches says whether a request carries this instance's token. Every route behind the
// token asks here, so the comparison takes as long whatever was sent, and no token can match "none".
func InstanceTokenMatches(o *root.Owner, r *http.Request) bool {
	sent := r.Header.Get("X-Localbase-Instance-Token")
	return o != nil && o.InstanceToken != "" && subtle.ConstantTimeCompare([]byte(sent), []byte(o.InstanceToken)) == 1
}
