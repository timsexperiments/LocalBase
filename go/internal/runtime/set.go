package runtime

// Set is a gateway's model runtimes, one of each kind; any of them is nil when the gateway has none.
type Set struct {
	LLM                    *LLM
	STT, TTS, Image, Video *Service
}
