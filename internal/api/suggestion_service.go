package api

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/tuning"
)

// suggestionService implements cairnos.v1.SuggestionService over the store's
// suggestion slice. It is review-only by design: scanning and replay stay on
// the CLI (replay spends real runner tokens), and nothing here writes
// workflow or config files; approval returns an annotation for the human to
// apply by hand.
type suggestionService struct {
	cairnosv1.UnimplementedSuggestionServiceServer

	store store.SuggestionStore
}

// ListSuggestions lists suggestions newest-first under the request filter.
func (s *suggestionService) ListSuggestions(ctx context.Context, req *cairnosv1.ListSuggestionsRequest) (*cairnosv1.ListSuggestionsResponse, error) {
	status, valid := suggestionStatusFromWire(req.GetStatus())
	if !valid {
		return nil, errorStatus(codes.InvalidArgument,
			cairnerr.Newf(codeInvalidArgument, "status %q is not one of open, approved, rejected, applied", req.GetStatus()))
	}

	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultListLimit
	}

	suggestions, err := s.store.ListSuggestions(ctx, store.SuggestionFilter{Status: status, Limit: limit})
	if err != nil {
		return nil, suggestionStatus(err)
	}

	out := make([]*cairnosv1.Suggestion, 0, len(suggestions))
	for _, suggestion := range suggestions {
		out = append(out, protoSuggestion(suggestion))
	}

	return &cairnosv1.ListSuggestionsResponse{Suggestions: out}, nil
}

// ApproveSuggestion marks a suggestion approved and returns the annotated
// change for manual application.
func (s *suggestionService) ApproveSuggestion(ctx context.Context, req *cairnosv1.ApproveSuggestionRequest) (*cairnosv1.ApproveSuggestionResponse, error) {
	suggestion, err := s.review(ctx, req.GetId(), store.SuggestionApproved)
	if err != nil {
		return nil, err
	}

	return &cairnosv1.ApproveSuggestionResponse{
		Suggestion: protoSuggestion(suggestion),
		Annotation: tuning.Annotation(suggestion),
	}, nil
}

// RejectSuggestion marks a suggestion rejected.
func (s *suggestionService) RejectSuggestion(ctx context.Context, req *cairnosv1.RejectSuggestionRequest) (*cairnosv1.RejectSuggestionResponse, error) {
	suggestion, err := s.review(ctx, req.GetId(), store.SuggestionRejected)
	if err != nil {
		return nil, err
	}

	return &cairnosv1.RejectSuggestionResponse{Suggestion: protoSuggestion(suggestion)}, nil
}

// review is the shared approve/reject spine: update the status, then return
// the updated row.
func (s *suggestionService) review(ctx context.Context, id string, status store.SuggestionStatus) (*store.Suggestion, error) {
	if id == "" {
		return nil, errorStatus(codes.InvalidArgument,
			cairnerr.New(codeInvalidArgument, "id is required"))
	}

	if err := s.store.UpdateSuggestionStatus(ctx, id, status); err != nil {
		return nil, suggestionStatus(err)
	}

	suggestion, err := s.store.GetSuggestion(ctx, id)
	if err != nil {
		return nil, suggestionStatus(err)
	}

	return suggestion, nil
}

// suggestionStatusFromWire maps the wire status filter onto the store type;
// empty means no filter.
func suggestionStatusFromWire(raw string) (store.SuggestionStatus, bool) {
	switch store.SuggestionStatus(raw) {
	case "":
		return "", true
	case store.SuggestionOpen, store.SuggestionApproved, store.SuggestionRejected, store.SuggestionApplied:
		return store.SuggestionStatus(raw), true
	default:
		return "", false
	}
}

// suggestionStatus classifies a suggestion store failure.
func suggestionStatus(err error) error {
	if errors.Is(err, store.ErrSuggestionNotFound) {
		return errorStatus(codes.NotFound, cairnerr.New(codeSuggestionNotFound, err.Error()))
	}

	return canonicalStatus(err)
}
