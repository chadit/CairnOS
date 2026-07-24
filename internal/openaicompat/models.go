package openaicompat

import "net/http"

// Wire-shape constants for GET /v1/models.
const (
	objectList  = "list"
	objectModel = "model"

	// modelOwner is the owned_by every adapter model reports. Personalities are
	// CairnOS-defined framings, not per-caller fine-tunes, so they share one
	// owner rather than carrying a user id.
	modelOwner = "cairnos"
)

// modelList is the GET /v1/models body: the personalities this node serves,
// each rendered as an OpenAI model object.
type modelList struct {
	Object string      `json:"object"`
	Data   []modelInfo `json:"data"`
}

// modelInfo is one model entry. ID is the personality name a client passes back
// as the completion's model field.
type modelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// handleModels is GET /v1/models: authorize under the same chat:session check as
// completions (frontends send the key here too), then list every personality as
// an OpenAI model. It ships in this cluster because several frontends query
// models before allowing a first chat (.agents/spec/parity/openai-compat.md,
// "GET /v1/models").
func (s *Service) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authorize(r.Context(), bearerFromHeader(r.Header)); err != nil {
		s.writeError(w, err)

		return
	}

	names := s.personalities.Names()
	data := make([]modelInfo, 0, len(names))

	for idx := range names {
		data = append(data, modelInfo{ID: names[idx], Object: objectModel, OwnedBy: modelOwner})
	}

	s.writeJSON(w, http.StatusOK, modelList{Object: objectList, Data: data})
}
