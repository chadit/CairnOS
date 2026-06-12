package api

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/rules"
)

// languageRulesService implements cairnos.v1.LanguageRulesService over the
// rules store.
type languageRulesService struct {
	cairnosv1.UnimplementedLanguageRulesServiceServer

	store rules.Service
}

// PutRule upserts one rule keyed by (language, project, key).
func (s *languageRulesService) PutRule(ctx context.Context, req *cairnosv1.PutRuleRequest) (*cairnosv1.PutRuleResponse, error) {
	if req.GetRule() == nil {
		return nil, errorStatus(codes.InvalidArgument,
			cairnerr.New(codeInvalidArgument, "rule is required"))
	}

	if err := s.store.PutRule(ctx, ruleFromProto(req.GetRule())); err != nil {
		return nil, rulesStatus(err)
	}

	return &cairnosv1.PutRuleResponse{}, nil
}

// GetRules returns the merged rule set for a language, with the project's
// overrides applied over the language defaults.
func (s *languageRulesService) GetRules(ctx context.Context, req *cairnosv1.GetRulesRequest) (*cairnosv1.GetRulesResponse, error) {
	set, err := s.store.GetRules(ctx, req.GetLanguage(), req.GetProject())
	if err != nil {
		return nil, rulesStatus(err)
	}

	merged := make([]*cairnosv1.LanguageRule, 0, len(set.Rules))
	for idx := range set.Rules {
		merged = append(merged, protoLanguageRule(&set.Rules[idx]))
	}

	return &cairnosv1.GetRulesResponse{Language: set.Language, Project: set.Project, Rules: merged}, nil
}

// ListLanguages returns the distinct languages with at least one rule.
func (s *languageRulesService) ListLanguages(ctx context.Context, _ *cairnosv1.ListLanguagesRequest) (*cairnosv1.ListLanguagesResponse, error) {
	langs, err := s.store.ListLanguages(ctx)
	if err != nil {
		return nil, rulesStatus(err)
	}

	return &cairnosv1.ListLanguagesResponse{Languages: langs}, nil
}

// rulesStatus classifies a rules store failure. An unknown language is a
// NOT_FOUND, an invalid rule an INVALID_ARGUMENT.
func rulesStatus(err error) error {
	switch {
	case errors.Is(err, rules.ErrUnknownLanguage):
		return errorStatus(codes.NotFound, cairnerr.New(codeUnknownLanguage, err.Error()))
	case errors.Is(err, rules.ErrInvalidRule):
		return errorStatus(codes.InvalidArgument, cairnerr.New(codeInvalidArgument, err.Error()))
	default:
		return canonicalStatus(err)
	}
}
