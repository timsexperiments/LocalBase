package httpx

const (
	// AudioTruncatedHeader marks speech cut at the runtime's frame cap (DESIGN §24).
	AudioTruncatedHeader = "X-Localbase-Audio-Truncated"
	// NoteAudioTruncated is the same flag in a detached run's result.notes.
	NoteAudioTruncated = "audio_truncated"
)
