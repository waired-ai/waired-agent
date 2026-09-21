package catalog

// Each engine keeps its own record of a model (waired-agent#1520): Models is
// the ollama engine's, VLLMModels the vLLM engine's. The two can both hold a
// model — a vLLM host measures its speed with a model its ollama engine may
// also serve — and one engine's record says nothing about the other, which
// a single row per model id could not keep apart.
//
// These accessors make every caller name the engine it means. A runtime they
// do not know answers "no record", and writes to it are dropped.

// EngineRuntimes is every engine that keeps model records, in a fixed order.
var EngineRuntimes = []string{RuntimeOllama, RuntimeVLLM}

// ModelsFor is runtime's records, nil for a runtime that keeps none. The map
// is the State's own: writing to it writes the State.
func (s State) ModelsFor(runtime string) map[string]ModelState {
	switch runtime {
	case RuntimeOllama:
		return s.Models
	case RuntimeVLLM:
		return s.VLLMModels
	}
	return nil
}

// ModelFor is runtime's record of model id.
func (s State) ModelFor(runtime, id string) (ModelState, bool) {
	ms, ok := s.ModelsFor(runtime)[id]
	return ms, ok
}

// SetModel writes runtime's record of model id.
func (s *State) SetModel(runtime, id string, ms ModelState) {
	switch runtime {
	case RuntimeOllama:
		if s.Models == nil {
			s.Models = map[string]ModelState{}
		}
		s.Models[id] = ms
	case RuntimeVLLM:
		if s.VLLMModels == nil {
			s.VLLMModels = map[string]ModelState{}
		}
		s.VLLMModels[id] = ms
	}
}

// RemoveModel drops runtime's record of model id.
func (s *State) RemoveModel(runtime, id string) {
	delete(s.ModelsFor(runtime), id)
}

// RecordsFor is every engine's record of model id, keyed by runtime: what a
// removal of the model has to take away.
func (s State) RecordsFor(id string) map[string]ModelState {
	out := map[string]ModelState{}
	for _, rt := range EngineRuntimes {
		if ms, ok := s.ModelFor(rt, id); ok {
			out[rt] = ms
		}
	}
	return out
}
