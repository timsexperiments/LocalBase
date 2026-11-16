package web

import (
	"encoding/json"
	"net/http"

	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
)

// The daily check, in Settings (DESIGN §33.1). It is read and changed through /_localbase/update as
// the signed-in person. There is no install button: an upgrade restarts the gateway serving this
// page, so it belongs on the computer.

func (u *UI) updateData(r *http.Request, notice, failure string) map[string]any {
	p := u.caller(r)
	data := map[string]any{"CanManage": u.d.NoAuth || usage.CanChange(p.Can), "Notice": notice, "Error": failure}
	res := u.call(r, "GET", "/_localbase/update", "", nil)
	var st upgrade.State
	if res.Status != http.StatusOK || json.Unmarshal(res.Body, &st) != nil {
		data["Unavailable"] = true
		return data
	}
	data["State"] = st
	return data
}

func (u *UI) updateCheck(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{"action": "set-check", "enabled": r.PostFormValue("enabled") == "on"}
	failure := u.apiCall(r, "POST", "/_localbase/update", body, nil)
	r.Header.Set("HX-Request", "true")
	r.Header.Set("HX-Target", "update-check")
	u.render(w, r, "settings", map[string]any{"Updates": u.updateData(r, "", failure)})
}
