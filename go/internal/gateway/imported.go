package gateway

import "context"

// importedTask is an imported model's LLM task, or "" when id isn't an imported LLM. It reads that
// model's row only, with ctx. An error is the library's, and never means "not imported": a caller
// answers it as the LLM service unavailable for now.
func (s *Server) importedTask(ctx context.Context, id string) (string, error) {
	if s.Library == nil {
		return "", nil
	}
	e, err := s.Library.Imported(ctx, id)
	if err != nil || e == nil {
		return "", err
	}
	return importTask(&e.Model), nil
}
