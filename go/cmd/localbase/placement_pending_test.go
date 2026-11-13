package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `models placement` says whether the next load differs with the running gateway's answer, which
// counts the context ceiling. Here the gateway says it does, though the split it reports loading
// with is the stored one, so working it out from the split alone, as the command line did, would
// say it doesn't.
func TestModelsPlacementTakesTheGatewaysPendingAnswer(t *testing.T) {
	roomyHost(t)
	e := readSitesEnv(t)
	model := e.Config.Active["llm"]
	for _, pending := range []bool{true, false} {
		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/_localbase/instance":
				json.NewEncoder(w).Encode(map[string]any{"instanceId": "id", "rootHash": "hash"})
			case "/_localbase/placement-state":
				json.NewEncoder(w).Encode(map[string]any{"loading": false, "loaded": true, "resolved": map[string]any{"mode": "auto"},
					"loadedRequest": map[string]any{"mode": "auto"}, "pendingNextLoad": pending})
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		host, port, _ := net.SplitHostPort(strings.TrimPrefix(gw.URL, "http://"))
		writeOwner(t, e, host, atoi(t, port), "id", "hash")
		code, out := runApp(t, "--root", e.Root.Dir, "--json", "models", "placement", model)
		gw.Close()
		var env struct {
			Data struct {
				PendingNextLoad bool `json:"pendingNextLoad"`
			} `json:"data"`
		}
		if code != 0 || json.NewDecoder(strings.NewReader(out[strings.Index(out, "{"):])).Decode(&env) != nil || env.Data.PendingNextLoad != pending {
			t.Errorf("the gateway says pending %v: exit %d, %s", pending, code, out)
		}
	}
}
