//go:build !localbase_lab

package gatewayrun

// labStopStuck does nothing outside a localbase_lab build (run_lab.go).
func labStopStuck() {}
