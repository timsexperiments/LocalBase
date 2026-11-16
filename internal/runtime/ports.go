package runtime

import "slices"

// MediaDefaultPorts are the speech, image and video backends' ports with no flag and no base.
// upgrade's survivor check reads them too.
var MediaDefaultPorts = map[string]int{"tts": 8092, "image": 8090, "video": 8091}

// PortOrder is the order of --runtime-port-base's block.
var PortOrder = []string{"llm", "stt", "tts", "image", "video"}

// PortInBlock is a runtime's port in the block from base, or configured with no base.
func PortInBlock(base int, kind string, configured int) int {
	if base <= 0 {
		return configured
	}
	return base + slices.Index(PortOrder, kind)
}
