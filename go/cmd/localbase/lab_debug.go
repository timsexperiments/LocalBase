//go:build localbase_lab

package main

import (
	"encoding/json"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"runtime/debug"
	runtimepprof "runtime/pprof"
)

// The reliability lab's view inside a gateway (go/tools/lab): a localbase_lab
// build only, never a release. When LOCALBASE_LAB_DEBUG_ADDR is set, this
// process serves its goroutine, file and heap counts and pprof on that
// address, for leak checks and for dumps when one fails.

const labDebugEnv = "LOCALBASE_LAB_DEBUG_ADDR"

func init() {
	if os.Getenv("LOCALBASE_LAB_STOP_STUCK") != "" {
		stopStuck = func() { select {} }
	}
	addr := os.Getenv(labDebugEnv)
	if addr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/free", func(w http.ResponseWriter, r *http.Request) { debug.FreeOSMemory() })
	mux.HandleFunc("/debug/stats", func(w http.ResponseWriter, r *http.Request) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		fds := -1
		if dir, err := os.Open("/dev/fd"); err == nil {
			// Names only: stat fails on the directory's own descriptor.
			if names, err := dir.Readdirnames(-1); err == nil {
				fds = len(names)
			}
			dir.Close()
		}
		json.NewEncoder(w).Encode(map[string]any{
			"goroutines": runtime.NumGoroutine(), "fds": fds, "threads": runtimepprof.Lookup("threadcreate").Count(),
			"heapInuse": m.HeapInuse, "heapAlloc": m.HeapAlloc, "heapObjects": m.HeapObjects, "sys": m.Sys, "numGC": m.NumGC})
	})
	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			println("lab debug listener:", err.Error())
		}
	}()
}
