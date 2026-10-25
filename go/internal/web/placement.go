package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// The Placement card on a model's page (DESIGN §20.2): where the model runs
// (GPU, CPU or split), what auto did and why, and the advanced modes. Copy
// is about capacity and CPU use only.

type placementSplit struct {
	Mode          string `json:"mode"`
	Platform      string `json:"platform"`
	GPULayers     int    `json:"gpuLayers"`
	TotalLayers   int    `json:"totalLayers"`
	CPUMoELayers  int    `json:"cpuMoeLayers"`
	HostTensors   int    `json:"hostTensors"`
	HostBytes     int64  `json:"hostBytes"`
	FreeVRAMBytes int64  `json:"freeVramBytes"`
	ReserveBytes  int64  `json:"reserveBytes"`
	Note          string `json:"note"`
	PlannedAt     string `json:"plannedAt"`
	ContextSize   int    `json:"contextSize"`
}

type placementView struct {
	ID              string
	Requested       map[string]any
	Loaded          bool
	Resolved        *placementSplit
	Next            *placementSplit
	NextError       string
	PendingNextLoad bool
	CanManage       bool
	Notice, Error   string
}

// Details, in words, of one split.
func (p *placementSplit) Lines() []string {
	if p == nil {
		return nil
	}
	gb := func(b int64) string { return strconv.FormatFloat(float64(b)/(1<<30), 'f', 1, 64) + " GB" }
	lines := []string{fmt.Sprintf("%d of %d layers on the GPU", p.GPULayers, p.TotalLayers)}
	if p.CPUMoELayers > 0 {
		lines = append(lines, fmt.Sprintf("Expert weights of layers 0–%d (%d tensors) in system memory", p.CPUMoELayers-1, p.HostTensors))
	}
	if p.HostBytes > 0 {
		lines = append(lines, gb(p.HostBytes)+" of system memory used by the model")
	}
	if p.Platform == "discrete-nvidia" && p.PlannedAt != "" {
		lines = append(lines, "Planned at "+p.PlannedAt+" against "+gb(p.FreeVRAMBytes)+" of free GPU memory ("+gb(p.ReserveBytes)+" kept free)")
	}
	return lines
}

func (u *UI) placementData(r *http.Request, id, notice, failure string) *placementView {
	v := &placementView{ID: id, CanManage: u.caller(r).Can("models:manage"), Notice: notice, Error: failure}
	res := u.call(r, "GET", "/_localbase/models/"+url.PathEscape(id)+"/placement", "", nil)
	if res.Status != 200 {
		if v.Error == "" {
			v.Error = res.apiError()
		}
		if v.Error == "" {
			return nil // no placement planning on this gateway
		}
		return v
	}
	var st struct {
		Requested       map[string]any            `json:"requested"`
		Loaded          bool                      `json:"loaded"`
		Resolved        *placementSplit           `json:"resolved"`
		Next            *placementSplit           `json:"next"`
		NextError       *struct{ Message string } `json:"nextError"`
		PendingNextLoad bool                      `json:"pendingNextLoad"`
	}
	if json.Unmarshal(res.Body, &st) != nil {
		return nil
	}
	v.Requested, v.Loaded, v.Resolved, v.Next, v.PendingNextLoad = st.Requested, st.Loaded, st.Resolved, st.Next, st.PendingNextLoad
	if st.NextError != nil {
		v.NextError = st.NextError.Message
	}
	return v
}

// placementPost saves the card's form through the API.
func (u *UI) placementPost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	fragment := func(notice, failure string) {
		r.Header.Set("HX-Request", "true")
		r.Header.Set("HX-Target", "placement-card-frag")
		u.render(w, r, "details", map[string]any{"Placement": u.placementData(r, id, notice, failure)})
	}
	method, body := "PUT", map[string]any{}
	if r.PostFormValue("action") == "reset" {
		method = "DELETE"
	} else {
		mode := r.PostFormValue("mode")
		body["mode"] = mode
		if mode == "custom" {
			for field, key := range map[string]string{"cpuMoeLayers": "cpu_moe_layers", "gpuLayers": "gpu_layers"} {
				if v := r.PostFormValue(key); v != "" {
					n, err := strconv.Atoi(v)
					if err != nil {
						fragment("", "Layer counts are whole numbers.")
						return
					}
					body[field] = n
				}
			}
		}
		if v := r.PostFormValue("context_size"); v != "" && v != r.PostFormValue("context_size_was") {
			n, err := strconv.Atoi(v)
			if err != nil {
				fragment("", "The context size is a whole number of tokens.")
				return
			}
			body["contextSize"] = n
		}
	}
	var b []byte
	if method == "PUT" {
		b, _ = json.Marshal(body)
	}
	res := u.call(r, method, "/_localbase/models/"+url.PathEscape(id)+"/placement", "application/json", b)
	if msg := res.apiError(); msg != "" || res.Status != 200 {
		if msg == "" {
			msg = "Could not save the placement."
		}
		fragment("", msg)
		return
	}
	fragment("Saved. It applies the next time the model loads.", "")
}

// ReqInt is a stored number for a form field ("" when unset; 0 stays 0).
func (v *placementView) ReqInt(name string) string {
	switch n := v.Requested[name].(type) {
	case float64:
		return strconv.Itoa(int(n))
	case int:
		return strconv.Itoa(n)
	}
	return ""
}
