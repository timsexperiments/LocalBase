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

func TestParseGPUMemory(t *testing.T) {
	got := parseGPUMemory([]byte("0, NVIDIA GeForce RTX 3060, 11899, 12288\n1, NVIDIA A10, 22000, 23028\nbad line\n"))
	if len(got) != 2 || got[0].Index != 0 || got[0].FreeBytes != 11899<<20 || got[0].TotalBytes != 12288<<20 || got[1].Name != "NVIDIA A10" {
		t.Errorf("parsed %+v", got)
	}
}
