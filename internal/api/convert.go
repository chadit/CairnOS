package api

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/rules"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/vault"
)

// protoRun maps a persisted run row onto the wire shape.
func protoRun(run *store.Run) *cairnosv1.Run {
	if run == nil {
		return nil
	}

	return &cairnosv1.Run{
		RunId:        run.ID,
		WorkflowName: run.WorkflowName,
		WorkflowHash: run.WorkflowHash,
		Status:       string(run.Status),
		Trigger:      run.Trigger,
		Inputs:       protoStruct(run.Inputs),
		Error:        protoError(run.Error),
		CreatedAt:    protoTime(run.CreatedAt),
		StartedAt:    protoTime(run.StartedAt),
		FinishedAt:   protoTime(run.FinishedAt),
	}
}

// protoActionResult maps a persisted action row onto the wire shape.
func protoActionResult(result *store.ActionResult) *cairnosv1.ActionResult {
	return &cairnosv1.ActionResult{
		Name:    result.ActionName,
		Runner:  result.Runner,
		Status:  string(result.Status),
		Attempt: int32(result.Attempt), // #nosec G115 -- attempts are small positive retry counts
		Output:  protoValue(result.Output),
		Usage: &cairnosv1.Usage{
			InputTokens:  result.Usage.InputTokens,
			OutputTokens: result.Usage.OutputTokens,
			CachedTokens: result.Usage.CachedTokens,
			CostUsd:      result.Usage.CostUSD,
			LatencyMs:    result.Usage.Latency.Milliseconds(),
			Retries:      int32(result.Usage.Retries), // #nosec G115 -- retry counts are small and positive
		},
		Error:      protoError(result.Error),
		StartedAt:  protoTime(result.StartedAt),
		FinishedAt: protoTime(result.FinishedAt),
	}
}

// protoEvent maps one engine event onto the stream wire shape. The
// run_finished payload is composed by the stream handler from the terminal
// run record instead, so this only covers run_started and transitions.
func protoEvent(evt *engine.Event) *cairnosv1.RunEvent {
	out := &cairnosv1.RunEvent{
		RunId: evt.RunID,
		Type:  protoEventType(evt.Type),
		Error: protoError(evt.Err),
	}

	if evt.Type == engine.EventActionTransition {
		out.Action = &cairnosv1.ActionState{
			Name:    evt.Action,
			Status:  evt.Status,
			Attempt: int32(evt.Attempt), // #nosec G115 -- attempts are small positive retry counts
		}
	}

	return out
}

// protoEventType maps the engine event names onto the proto enum.
func protoEventType(eventType engine.EventType) cairnosv1.RunEventType {
	switch eventType {
	case engine.EventRunStarted:
		return cairnosv1.RunEventType_RUN_EVENT_TYPE_RUN_STARTED
	case engine.EventActionTransition:
		return cairnosv1.RunEventType_RUN_EVENT_TYPE_ACTION_TRANSITION
	case engine.EventRunFinished:
		return cairnosv1.RunEventType_RUN_EVENT_TYPE_RUN_FINISHED
	default:
		return cairnosv1.RunEventType_RUN_EVENT_TYPE_UNSPECIFIED
	}
}

// protoSuggestion maps a persisted tuning suggestion onto the wire shape.
func protoSuggestion(suggestion *store.Suggestion) *cairnosv1.Suggestion {
	return &cairnosv1.Suggestion{
		Id:        suggestion.ID,
		Kind:      string(suggestion.Kind),
		Target:    suggestion.Target,
		Status:    string(suggestion.Status),
		Rationale: suggestion.Rationale,
		Evidence:  protoStruct(suggestion.Evidence),
		Proposed:  protoStruct(suggestion.Proposed),
		EstSavings: &cairnosv1.Savings{
			CostUsdPerRun:  suggestion.EstSavings.CostUSDPerRun,
			LatencyDeltaMs: suggestion.EstSavings.LatencyDelta.Milliseconds(),
			Confidence:     suggestion.EstSavings.Confidence,
		},
		CreatedAt: protoTime(suggestion.CreatedAt),
	}
}

// protoMemoryEntry maps a memory entry onto the wire shape.
func protoMemoryEntry(entry *memory.Entry) *cairnosv1.MemoryEntry {
	return &cairnosv1.MemoryEntry{
		Id:         entry.ID,
		Scope:      entry.Scope,
		Content:    entry.Content,
		Importance: int32(entry.Importance), // #nosec G115 -- importance is bounded 0..100
		Tags:       entry.Tags,
		TokenCost:  int32(entry.TokenCost), // #nosec G115 -- token costs stay far below int32
		CreatedAt:  protoTime(entry.CreatedAt),
		UpdatedAt:  protoTime(entry.UpdatedAt),
	}
}

// memoryEntryFromProto maps a wire entry onto the domain shape. Timestamps
// are stamped by the store, so only the writable fields cross.
func memoryEntryFromProto(wire *cairnosv1.MemoryEntry) *memory.Entry {
	return &memory.Entry{
		ID:         wire.GetId(),
		Scope:      wire.GetScope(),
		Content:    wire.GetContent(),
		Importance: int(wire.GetImportance()),
		Tags:       wire.GetTags(),
		TokenCost:  int(wire.GetTokenCost()),
	}
}

// protoVaultDocument maps a vault document onto the wire shape, including the
// derived backlinks a Get carries.
func protoVaultDocument(doc *vault.Document) *cairnosv1.VaultDocument {
	return &cairnosv1.VaultDocument{
		Id:        doc.ID,
		Title:     doc.Title,
		Body:      doc.Body,
		Tags:      doc.Tags,
		Links:     doc.Links,
		Backlinks: doc.Backlinks,
		CreatedAt: protoTime(doc.CreatedAt),
		UpdatedAt: protoTime(doc.UpdatedAt),
	}
}

// vaultDocumentFromProto maps a wire document onto the domain shape. Backlinks
// are derived and timestamps are store-stamped, so neither crosses on a put.
func vaultDocumentFromProto(wire *cairnosv1.VaultDocument) *vault.Document {
	return &vault.Document{
		ID:    wire.GetId(),
		Title: wire.GetTitle(),
		Body:  wire.GetBody(),
		Tags:  wire.GetTags(),
		Links: wire.GetLinks(),
	}
}

// protoLanguageRule maps a merged rule onto the wire shape.
func protoLanguageRule(rule *rules.Rule) *cairnosv1.LanguageRule {
	return &cairnosv1.LanguageRule{
		Language:    rule.Language,
		Project:     rule.Project,
		Key:         rule.Key,
		Value:       rule.Value,
		Description: rule.Description,
	}
}

// ruleFromProto maps a wire rule onto the domain shape.
func ruleFromProto(wire *cairnosv1.LanguageRule) *rules.Rule {
	return &rules.Rule{
		Language:    wire.GetLanguage(),
		Project:     wire.GetProject(),
		Key:         wire.GetKey(),
		Value:       wire.GetValue(),
		Description: wire.GetDescription(),
	}
}

// protoTime renders a timestamp, with nil for the zero time so "not yet"
// fields stay absent from JSON instead of rendering the Unix epoch.
func protoTime(value time.Time) *timestamppb.Timestamp {
	if value.IsZero() {
		return nil
	}

	return timestamppb.New(value)
}

// protoAuditEntry maps a persisted governance audit row onto the wire shape.
func protoAuditEntry(entry *store.AuditEntry) *cairnosv1.AuditEntry {
	return &cairnosv1.AuditEntry{
		Id:        entry.ID,
		Actor:     entry.Actor,
		Action:    entry.Action,
		Target:    entry.Target,
		Detail:    entry.Detail,
		CreatedAt: protoTime(entry.CreatedAt),
	}
}

// protoStruct renders a JSON-shaped map, dropping it when it cannot become a
// Struct; run inputs and outputs are JSON-shaped by the runner contract, so
// failure here means a programming error upstream, not a request problem.
func protoStruct(values map[string]any) *structpb.Struct {
	if len(values) == 0 {
		return nil
	}

	converted, err := structpb.NewStruct(values)
	if err != nil {
		return nil
	}

	return converted
}

// protoValue renders one action output. Non-JSON values degrade to their
// string rendering rather than vanishing, so history stays inspectable.
func protoValue(value any) *structpb.Value {
	if value == nil {
		return nil
	}

	converted, err := structpb.NewValue(value)
	if err != nil {
		return structpb.NewStringValue(fmt.Sprintf("%v", value))
	}

	return converted
}
