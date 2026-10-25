package memory

import "testing"

func TestParseNvidiaSMI(t *testing.T) {
	got := parseNvidiaSMI([]byte("NVIDIA GeForce RTX 3060, 12288\nNVIDIA A10, 23028\n\n"))
	if len(got) != 2 || got[0] != (NvidiaGPU{"NVIDIA GeForce RTX 3060", 12288}) || got[1].MemoryMiB != 23028 {
		t.Errorf("parsed %+v", got)
	}
	if parseNvidiaSMI([]byte("")) != nil || parseNvidiaSMI([]byte("No devices were found")) != nil {
		t.Error("parsed GPUs from no output")
	}
}
