package runtime

import "testing"

func TestPortInBlock(t *testing.T) {
	if got := PortInBlock(0, "stt", 8081); got != 8081 {
		t.Errorf("no base: %d", got)
	}
	for i, name := range []string{"llm", "stt", "tts", "image", "video"} {
		if got := PortInBlock(30000, name, 8081); got != 30000+i {
			t.Errorf("%s: %d", name, got)
		}
	}
}

// The default ports, which upgrade's survivor check also reads.
func TestMediaDefaultPorts(t *testing.T) {
	if MediaDefaultPorts["tts"] != 8092 || MediaDefaultPorts["image"] != 8090 || MediaDefaultPorts["video"] != 8091 || len(MediaDefaultPorts) != 3 {
		t.Errorf("%v", MediaDefaultPorts)
	}
}
