//go:build localbase_lab

package gatewayrun

import "os"

// labStopStuck is a stop that never returns when LOCALBASE_LAB_STOP_STUCK is set, in a localbase_lab
// build only, to test what happens then (go/test/lab).
func labStopStuck() {
	if os.Getenv("LOCALBASE_LAB_STOP_STUCK") != "" {
		select {}
	}
}
