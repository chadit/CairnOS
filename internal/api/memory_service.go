package api

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/memory"
)

// memoryService implements cairnos.v1.MemoryService over the memory store.
type memoryService struct {
	cairnosv1.UnimplementedMemoryServiceServer

	store memory.Store
}

// AddMemory persists a new entry and returns its id.
func (s *memoryService) AddMemory(ctx context.Context, req *cairnosv1.AddMemoryRequest) (*cairnosv1.AddMemoryResponse, error) {
	if req.GetEntry() == nil {
		return nil, errorStatus(codes.InvalidArgument,
			cairnerr.New(codeInvalidArgument, "entry is required"))
	}

	id, err := s.store.Add(ctx, memoryEntryFromProto(req.GetEntry()))
	if err != nil {
		return nil, memoryStatus(err)
	}

	return &cairnosv1.AddMemoryResponse{Id: id}, nil
}

// GetMemory fetches one entry by id.
func (s *memoryService) GetMemory(ctx context.Context, req *cairnosv1.GetMemoryRequest) (*cairnosv1.GetMemoryResponse, error) {
	entry, err := s.store.Get(ctx, req.GetId())
	if err != nil {
		return nil, memoryStatus(err)
	}

	return &cairnosv1.GetMemoryResponse{Entry: protoMemoryEntry(&entry)}, nil
}

// ReplaceMemory overwrites one entry's writable fields.
func (s *memoryService) ReplaceMemory(ctx context.Context, req *cairnosv1.ReplaceMemoryRequest) (*cairnosv1.ReplaceMemoryResponse, error) {
	if req.GetEntry() == nil {
		return nil, errorStatus(codes.InvalidArgument,
			cairnerr.New(codeInvalidArgument, "entry is required"))
	}

	if err := s.store.Replace(ctx, req.GetId(), memoryEntryFromProto(req.GetEntry())); err != nil {
		return nil, memoryStatus(err)
	}

	updated, err := s.store.Get(ctx, req.GetId())
	if err != nil {
		return nil, memoryStatus(err)
	}

	return &cairnosv1.ReplaceMemoryResponse{Entry: protoMemoryEntry(&updated)}, nil
}

// DeleteMemory removes one entry by id.
func (s *memoryService) DeleteMemory(ctx context.Context, req *cairnosv1.DeleteMemoryRequest) (*cairnosv1.DeleteMemoryResponse, error) {
	if err := s.store.Delete(ctx, req.GetId()); err != nil {
		return nil, memoryStatus(err)
	}

	return &cairnosv1.DeleteMemoryResponse{}, nil
}

// SearchMemory returns entries relevant to the query, with the semantic
// fallback flag surfaced so callers know what kind of search answered.
func (s *memoryService) SearchMemory(ctx context.Context, req *cairnosv1.SearchMemoryRequest) (*cairnosv1.SearchMemoryResponse, error) {
	result, err := s.store.Search(ctx, memory.Query{
		Scope:    req.GetScope(),
		Text:     req.GetText(),
		Tags:     req.GetTags(),
		Limit:    int(req.GetLimit()),
		Semantic: req.GetSemantic(),
	})
	if err != nil {
		return nil, memoryStatus(err)
	}

	entries := make([]*cairnosv1.MemoryEntry, 0, len(result.Entries))
	for idx := range result.Entries {
		entries = append(entries, protoMemoryEntry(&result.Entries[idx]))
	}

	return &cairnosv1.SearchMemoryResponse{Entries: entries, Fallback: result.Fallback}, nil
}

// memoryStatus classifies a memory store failure.
func memoryStatus(err error) error {
	if errors.Is(err, memory.ErrEntryNotFound) {
		return errorStatus(codes.NotFound, cairnerr.New(codeMemoryNotFound, err.Error()))
	}

	return canonicalStatus(err)
}
