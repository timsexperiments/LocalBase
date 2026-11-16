package models

// ImportTask is the LLM task an imported model serves, chat, embeddings or decision, or "" for none.
func ImportTask(m *Model) string {
	for _, t := range []string{"chat", "embeddings", "decision"} {
		if m.HasTask(t) {
			return t
		}
	}
	return ""
}
