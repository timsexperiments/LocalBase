package main

import (
	"slices"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// upgrade's survivor check looks for backends where serve put them: --x-port in either form, and
// --runtime-port-base's block, as serve resolves them (review 01-M11). Before, it saw only
// "--x-port value", ignored the base, and assumed 8090-8092 for image, video and speech.
func TestUpgradeChecksTheBackendPortsServeUses(t *testing.T) {
	all := map[string]string{"llm": "m", "stt": "m", "tts": "m", "image": "m", "video": "m"}
	e := &env{Config: &store.Config{Port: 18000, SttPort: 18001, Active: all}}
	for _, c := range []struct {
		args []string
		want []int
	}{
		{[]string{"serve"}, []int{8090, 8091, 8092, 18000, 18001}},
		{[]string{"serve", "--image-port=9001", "--tts-port", "9002"}, []int{8091, 9001, 9002, 18000, 18001}},
		{[]string{"serve", "--runtime-port-base", "30000"}, []int{30000, 30001, 30002, 30003, 30004}},
		{[]string{"serve", "--runtime-port-base=30000", "--video-port=9005"}, []int{9005, 30000, 30001, 30002, 30003}},
		{[]string{"serve", "--llm-port", "9010", "--runtime-port-base", "30000"}, []int{9010, 30001, 30002, 30003, 30004}},
	} {
		got := runtime.BackendPorts(e.Config, c.args)
		slices.Sort(got)
		if !slices.Equal(got, c.want) {
			t.Errorf("%v: %v, want %v", c.args, got, c.want)
		}
	}
}
