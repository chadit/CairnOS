package cli

import (
	"context"
	"flag"
	"path/filepath"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/hostenv"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/tuning"
	"github.com/chadit/CairnOS/internal/workflow"
)

const suggestionsUsage = "usage: cairnos suggestions <scan|list|approve|reject|apply|revert> [flags]\n"

// replayRunWindow caps how many recent runs the replay sample builder walks
// when reconstructing recorded inputs for one suggestion.
const replayRunWindow = 20

// cmdSuggestions dispatches the Phase 3 review-flow subcommands
// (.agents/spec/06-observability-and-tuning.md, "Review flow").
func (a *App) cmdSuggestions(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", suggestionsUsage)

		return ExitUsage
	}

	switch args[0] {
	case "scan":
		return a.cmdSuggestionsScan(ctx, args[1:])
	case subcmdList:
		return a.cmdSuggestionsList(ctx, args[1:])
	case "approve":
		return a.cmdSuggestionsApprove(ctx, args[1:])
	case "reject":
		return a.cmdSuggestionsReject(ctx, args[1:])
	case "apply":
		return a.cmdSuggestionsApply(ctx, args[1:])
	case "revert":
		return a.cmdSuggestionsRevert(ctx, args[1:])
	default:
		a.printErrf("unknown suggestions subcommand %q\n", args[0])
		a.printErrf("%s", suggestionsUsage)

		return ExitUsage
	}
}

// cmdSuggestionsScan runs the detectors over run history, persists what
// clears the savings floor, and prints the queue. Replay is opt-in via
// --replay because it re-invokes real runners with real cost; without it,
// output-changing suggestions carry a skipped replay marker instead.
func (a *App) cmdSuggestionsScan(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("suggestions scan", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	sinceFlag := flags.String("since", "", "only scan runs created within this window (e.g. 30d)")
	replayFlag := flags.Bool("replay", false, "replay output-changing suggestions against recorded inputs (re-invokes runners; costs real tokens)")
	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	var since time.Time

	if *sinceFlag != "" {
		window, err := workflow.ParseDuration(*sinceFlag)
		if err != nil {
			a.printErrf("suggestions scan: --since: %v\n", err)

			return ExitUsage
		}

		since = time.Now().Add(-window)
	}

	db, cfg, code, proceed := a.openStoreAt(ctx, "suggestions scan", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	tuneCfg := tuning.DefaultConfig()
	tuneCfg.MinSavingsUSD = cfg.Tuning.MinSavingsUSD
	tuneCfg.RunnerConfigs = cfg.Runners
	tuneCfg.Since = since
	tuneCfg.NewID = store.NewID

	if *replayFlag {
		// Replay analysis re-executes prior actions to score tuning suggestions;
		// it opens no secret source, so outbound credentials are not wired here.
		// A workflow that binds a credential to a runner fails this replay at
		// BuildAll, which is the same signal as a missing credential elsewhere.
		registry, err := newRegistry(ctx, cfg.Runners, sourceSpecs(cfg.Sources), hostenv.Render(cfg.Hosts), nil, nil, nil, nil,
			a.logger, nil)
		if err != nil {
			a.printErrf("suggestions scan: build runners for replay: %v\n", err)

			return ExitFailure
		}
		defer a.closeRegistry(registry)

		tuneCfg.Runners = registry

		// With an embedding model configured, replay judges output equivalence
		// by embedding similarity instead of exact match (R1); otherwise the
		// engine keeps its exact-match default.
		if comparator := replayComparator(cfg.Tuning.EmbeddingModel, cfg.Tuning.EquivalenceThreshold, cfg.Runners); comparator != nil {
			tuneCfg.Comparator = comparator
		}
	}

	engine := tuning.New(db, tuneCfg)

	suggestions, err := engine.Scan(ctx)
	if err != nil {
		a.printErrf("suggestions scan: %v\n", err)

		return ExitFailure
	}

	if *replayFlag {
		if code := a.replaySuggestions(ctx, engine, db, *dirFlag, tuneCfg.ReplayMaxSamples, suggestions); code != ExitOK {
			return code
		}
	}

	for _, suggestion := range suggestions {
		if err := db.SaveSuggestion(ctx, suggestion); err != nil {
			a.printErrf("suggestions scan: save: %v\n", err)

			return ExitFailure
		}
	}

	if len(suggestions) == 0 {
		a.printOutf("no suggestions cleared the savings floor\n")

		return ExitOK
	}

	a.writeSuggestionTable(suggestions)

	return ExitOK
}

// replayComparator builds the output-equivalence comparator for replay. An
// empty embedding model returns nil, so the tuning engine keeps its exact-match
// default; a configured model returns the embedding-similarity comparator (R1),
// reusing the openai runner's base URL and API key.
func replayComparator(model string, threshold float64, runners map[string]map[string]any) tuning.Comparator {
	if model == "" {
		return nil
	}

	embedder := builtins.NewOpenAIEmbedder(model, openaiBaseURL(runners), nil)

	return tuning.NewEmbeddingComparator(embedder, threshold)
}

// openaiBaseURL returns the openai runner's configured base_url so the embedder
// targets the same endpoint a proxy or gateway points at, or "" for the hosted
// default.
func openaiBaseURL(runners map[string]map[string]any) string {
	openai, configured := runners[builtins.OpenAIName]
	if !configured {
		return ""
	}

	base, _ := openai["base_url"].(string)

	return base
}

// replaySuggestions runs the replay guardrail over every output-changing
// suggestion in the batch. Sample reconstruction can come up empty (workflow
// file gone, outputs pruned); Replay then records a skipped marker on the
// suggestion rather than pretending the check ran.
func (a *App) replaySuggestions(ctx context.Context, engine *tuning.Engine, db store.Store, dirFlagValue string, sampleLimit int, suggestions []*store.Suggestion) int {
	home, err := resolveDir(dirFlagValue)
	if err != nil {
		a.printErrf("suggestions scan: %v\n", err)

		return ExitFailure
	}

	for _, suggestion := range suggestions {
		if !tuning.OutputChanging(suggestion.Kind) {
			continue
		}

		samples, sampleErr := a.buildReplaySamples(ctx, db, home, suggestion, sampleLimit)
		if sampleErr != nil {
			a.printErrf("suggestions scan: replay samples for %s: %v\n", suggestion.Target, sampleErr)

			return ExitFailure
		}

		if replayErr := engine.Replay(ctx, suggestion, samples); replayErr != nil {
			a.printErrf("suggestions scan: replay %s: %v\n", suggestion.Target, replayErr)

			return ExitFailure
		}
	}

	return ExitOK
}

// buildReplaySamples reconstructs up to limit recorded executions for one
// suggestion: params come from the action's definition in the workflows
// directory (params are not persisted), inputs and outputs come from run
// history. An empty result is not an error; Replay records it as skipped.
func (a *App) buildReplaySamples(ctx context.Context, db store.Store, home string, suggestion *store.Suggestion, limit int) ([]tuning.Sample, error) {
	workflowName, _ := suggestion.Evidence[tuning.EvidenceWorkflow].(string)
	actionName, _ := suggestion.Evidence[tuning.EvidenceAction].(string)

	if workflowName == "" || actionName == "" {
		return nil, nil
	}

	params := a.lookupActionParams(filepath.Join(home, workflowsDirName), workflowName, actionName)
	if params == nil {
		return nil, nil
	}

	runs, err := db.ListRuns(ctx, store.ListFilter{WorkflowName: workflowName, Limit: replayRunWindow})
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller prefixes the suggestion target
	}

	var samples []tuning.Sample

	for _, run := range runs {
		if len(samples) >= limit {
			break
		}

		results, listErr := db.ListActionResults(ctx, run.ID)
		if listErr != nil {
			return nil, listErr //nolint:wrapcheck // the caller prefixes the suggestion target
		}

		for _, result := range results {
			if result.ActionName != actionName || result.Status != store.ActionSucceeded || result.Output == nil {
				continue
			}

			samples = append(samples, tuning.Sample{
				Params:         params,
				Inputs:         run.Inputs,
				RecordedOutput: result.Output,
			})

			break
		}
	}

	return samples, nil
}

// lookupActionParams finds the named action's params in the workflows
// directory, scanning every TOML file for the matching workflow name. A nil
// return means the definition is gone or unreadable, which downgrades replay
// to skipped rather than failing the scan: history outliving its workflow
// file is normal.
func (a *App) lookupActionParams(workflowsDir, workflowName, actionName string) map[string]any {
	entries, err := filepath.Glob(filepath.Join(workflowsDir, "*.toml"))
	if err != nil {
		a.logger.Warn("scan workflows directory", "error", err)

		return nil
	}

	for _, path := range entries {
		flow, loadErr := workflow.Load(path)
		if loadErr != nil || flow.Name != workflowName {
			continue
		}

		action, exists := flow.Actions[actionName]
		if !exists {
			return nil
		}

		return action.Params
	}

	return nil
}

// cmdSuggestionsList prints the persisted queue, filterable by status.
func (a *App) cmdSuggestionsList(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("suggestions list", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	statusFlag := flags.String("status", "", "narrow to one status: open, approved, or rejected")
	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	status, valid := parseSuggestionStatus(*statusFlag)
	if !valid {
		a.printErrf("suggestions list: --status must be open, approved, or rejected\n")

		return ExitUsage
	}

	db, _, code, proceed := a.openStoreAt(ctx, "suggestions list", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	suggestions, err := db.ListSuggestions(ctx, store.SuggestionFilter{Status: status, Limit: defaultListLimit})
	if err != nil {
		a.printErrf("suggestions list: %v\n", err)

		return ExitFailure
	}

	if len(suggestions) == 0 {
		a.printOutf("no suggestions\n")

		return ExitOK
	}

	a.writeSuggestionTable(suggestions)

	return ExitOK
}

// cmdSuggestionsApprove marks a suggestion approved and prints the annotated
// change for the human to apply by hand. Nothing writes workflow or config
// files; that is the Phase 3 contract.
func (a *App) cmdSuggestionsApprove(ctx context.Context, args []string) int {
	return a.reviewSuggestion(ctx, "suggestions approve", store.SuggestionApproved, args)
}

// cmdSuggestionsReject marks a suggestion rejected.
func (a *App) cmdSuggestionsReject(ctx context.Context, args []string) int {
	return a.reviewSuggestion(ctx, "suggestions reject", store.SuggestionRejected, args)
}

// reviewSuggestion is the shared approve/reject spine: update the status,
// then show the reviewer what they decided on.
func (a *App) reviewSuggestion(ctx context.Context, label string, status store.SuggestionStatus, args []string) int {
	flags := flag.NewFlagSet(label, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	id, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if id == "" {
		a.printErrf("usage: cairnos %s <id> [--dir D]\n", label)

		return ExitUsage
	}

	db, _, code, proceed := a.openStoreAt(ctx, label, *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	if err := db.UpdateSuggestionStatus(ctx, id, status); err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	if status == store.SuggestionRejected {
		a.printOutf("rejected %s\n", id)

		return ExitOK
	}

	suggestion, err := db.GetSuggestion(ctx, id)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	a.printOutf("approved %s\n\n%s", id, tuning.Annotation(suggestion))

	return ExitOK
}

// parseSuggestionStatus maps the --status flag onto the store type; empty
// means no filter.
func parseSuggestionStatus(raw string) (store.SuggestionStatus, bool) {
	switch store.SuggestionStatus(raw) {
	case "":
		return "", true
	case store.SuggestionOpen, store.SuggestionApproved, store.SuggestionRejected, store.SuggestionApplied:
		return store.SuggestionStatus(raw), true
	default:
		return "", false
	}
}

// writeSuggestionTable renders the queue with the evidence-bearing columns a
// reviewer triages by.
func (a *App) writeSuggestionTable(suggestions []*store.Suggestion) {
	tab := newTab(a.stdout)

	a.tabWritef(tab, "ID\tKIND\tTARGET\tSTATUS\tCONFIDENCE\tEST $/RUN\tRATIONALE\n")

	for _, suggestion := range suggestions {
		a.tabWritef(tab, "%s\t%s\t%s\t%s\t%s\t$%.4f\t%s\n",
			suggestion.ID, suggestion.Kind, suggestion.Target, suggestion.Status,
			suggestion.EstSavings.Confidence, suggestion.EstSavings.CostUSDPerRun,
			truncate(strings.ReplaceAll(suggestion.Rationale, "\n", " ")))
	}

	a.flushTab(tab)
}
