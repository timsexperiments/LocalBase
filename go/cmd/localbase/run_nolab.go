//go:build !localbase_lab

package main

// labStopStuck does nothing outside a localbase_lab build (run_lab.go).
func labStopStuck() {}
