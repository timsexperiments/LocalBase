package runtime

import (
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// BackendPorts are where this root's backends listen, as configured. A flag can move the media
// ones, and those are read from how the service is started. Only a kind this root uses counts:
// another root's backend on a default port is not one this gateway started, and could not be
// told apart from a survivor.
func BackendPorts(cfg *store.Config, arguments []string) []int {
	// serve's flags as given: "--x value" or "--x=value".
	flag := func(name string) (int, bool) {
		for i, a := range arguments {
			if v, ok := strings.CutPrefix(a, "--"+name+"="); ok {
				n, err := strconv.Atoi(v)
				return n, err == nil
			}
			if a == "--"+name && i+1 < len(arguments) {
				n, err := strconv.Atoi(arguments[i+1])
				return n, err == nil
			}
		}
		return 0, false
	}
	// The same resolution as serve's: an explicit port, else --runtime-port-base's block, else the
	// configured or default port.
	base, _ := flag("runtime-port-base")
	defaults := map[string]int{"llm": cfg.Port, "stt": cfg.SttPort}
	for kind, p := range MediaDefaultPorts {
		defaults[kind] = p
	}
	out := make([]int, 0, len(PortOrder))
	for _, kind := range PortOrder {
		p, explicit := flag(kind + "-port")
		if !explicit {
			p = PortInBlock(base, kind, defaults[kind])
		}
		if p > 0 && (len(cfg.Selected[kind]) > 0 || cfg.Active[kind] != "") {
			out = append(out, p)
		}
	}
	return out
}

// PortListening reports whether something accepts a connection on the loopback port.
func PortListening(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
