package api

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/vault"
)

// vaultService implements cairnos.v1.VaultService over the vault store.
type vaultService struct {
	cairnosv1.UnimplementedVaultServiceServer

	store vault.Store
}

// PutDocument persists a new document or overwrites an existing one.
func (s *vaultService) PutDocument(ctx context.Context, req *cairnosv1.PutDocumentRequest) (*cairnosv1.PutDocumentResponse, error) {
	if req.GetDocument() == nil {
		return nil, errorStatus(codes.InvalidArgument,
			cairnerr.New(codeInvalidArgument, "document is required"))
	}

	id, err := s.store.Put(ctx, vaultDocumentFromProto(req.GetDocument()))
	if err != nil {
		return nil, vaultStatus(err)
	}

	return &cairnosv1.PutDocumentResponse{Id: id}, nil
}

// GetDocument fetches one document by id, with its derived backlinks.
func (s *vaultService) GetDocument(ctx context.Context, req *cairnosv1.GetDocumentRequest) (*cairnosv1.GetDocumentResponse, error) {
	doc, err := s.store.Get(ctx, req.GetId())
	if err != nil {
		return nil, vaultStatus(err)
	}

	return &cairnosv1.GetDocumentResponse{Document: protoVaultDocument(&doc)}, nil
}

// DeleteDocument removes one document and its links.
func (s *vaultService) DeleteDocument(ctx context.Context, req *cairnosv1.DeleteDocumentRequest) (*cairnosv1.DeleteDocumentResponse, error) {
	if err := s.store.Delete(ctx, req.GetId()); err != nil {
		return nil, vaultStatus(err)
	}

	return &cairnosv1.DeleteDocumentResponse{}, nil
}

// QueryVault runs the typed graph query and surfaces the semantic fallback
// flag so callers know which kind of search answered.
func (s *vaultService) QueryVault(ctx context.Context, req *cairnosv1.QueryVaultRequest) (*cairnosv1.QueryVaultResponse, error) {
	result, err := s.store.Query(ctx, vault.Query{
		Tags:       req.GetTags(),
		Project:    req.GetProject(),
		LinkedFrom: req.GetLinkedFrom(),
		Hops:       int(req.GetHops()),
		Text:       req.GetText(),
		Limit:      int(req.GetLimit()),
		Semantic:   req.GetSemantic(),
	})
	if err != nil {
		return nil, vaultStatus(err)
	}

	documents := make([]*cairnosv1.VaultDocument, 0, len(result.Documents))
	for idx := range result.Documents {
		documents = append(documents, protoVaultDocument(&result.Documents[idx]))
	}

	return &cairnosv1.QueryVaultResponse{Documents: documents, Fallback: result.Fallback}, nil
}

// vaultStatus classifies a vault store failure.
func vaultStatus(err error) error {
	if errors.Is(err, vault.ErrDocumentNotFound) {
		return errorStatus(codes.NotFound, cairnerr.New(codeDocumentNotFound, err.Error()))
	}

	return canonicalStatus(err)
}
