// Typed mirror of the CairnOS HTTP gateway surface, hand-written against
// internal/api/gen/openapi/cairnos.swagger.json. See web/README.md for why
// these are hand-written rather than generated: the swagger is OpenAPI 2.0
// with several freeform `object` fields (run inputs, action output, error
// details) that a generator renders as opaque `unknown`, and the streaming
// NDJSON response has no first-class swagger shape. Modeling them by hand
// keeps the call sites honestly typed.

/** JSON value, the safe stand-in for the gateway's freeform `object` fields. */
export type JsonValue = string | number | boolean | null | JsonValue[] | JsonObject;

/**
 * JsonObject is a freeform JSON map, e.g. run inputs or error details. An
 * interface (not a Record alias) keeps the recursive type from collapsing to
 * `any` under typescript-eslint's type-aware checks.
 */
export interface JsonObject {
  [key: string]: JsonValue;
}

/**
 * ApiError is the canonical engine error from the error envelope
 * {status:"error", error:{code,message,details}}. Every failed call surfaces
 * one of these so a caller always reads the same shape.
 */
export interface ApiError {
  code: string;
  message: string;
  details?: JsonObject;
}

/** LoginResponse carries the bearer token and its expiry. */
export interface LoginResponse {
  token: string;
  expiresAt?: string;
}

/** HealthResponse reports gateway liveness. `status` is "ok" when ready. */
export interface HealthResponse {
  status: string;
}

/** RunnerInfo names one registered runner. */
export interface RunnerInfo {
  name: string;
}

export interface ListRunnersResponse {
  runners: RunnerInfo[];
}

/** Run lifecycle states, mirroring the engine status vocabulary. */
export type RunStatus = 'pending' | 'running' | 'succeeded' | 'failed' | 'canceled';

/** Run is one execution of a workflow. */
export interface Run {
  runId: string;
  workflowName: string;
  workflowHash?: string;
  status: string;
  trigger?: string;
  inputs?: JsonObject;
  error?: ApiError;
  createdAt?: string;
  startedAt?: string;
  finishedAt?: string;
}

export interface ListRunsResponse {
  runs: Run[];
}

/** Usage is the per-action resource accounting a runner reports. */
export interface Usage {
  inputTokens?: string;
  outputTokens?: string;
  cachedTokens?: string;
  costUsd?: number;
  latencyMs?: string;
  retries?: number;
}

/** ActionResult is the persisted outcome of one action within a run. */
export interface ActionResult {
  name: string;
  runner?: string;
  status: string;
  attempt?: number;
  output?: JsonValue;
  usage?: Usage;
  error?: ApiError;
  startedAt?: string;
  finishedAt?: string;
}

export interface GetRunResponse {
  run: Run;
  actions?: ActionResult[];
}

export interface CancelRunResponse {
  runId: string;
  status: string;
}

/** ActionState is one action's position within a run at event time. */
export interface ActionState {
  name: string;
  status: string;
  attempt?: number;
}

/** RunEventType says what a RunEvent describes. */
export type RunEventType =
  | 'RUN_EVENT_TYPE_UNSPECIFIED'
  | 'RUN_EVENT_TYPE_RUN_STARTED'
  | 'RUN_EVENT_TYPE_ACTION_TRANSITION'
  | 'RUN_EVENT_TYPE_RUN_FINISHED';

/** RunEvent is one entry in the RunWorkflow stream. */
export interface RunEvent {
  runId: string;
  type: RunEventType;
  action?: ActionState;
  run?: Run;
  error?: ApiError;
}

/**
 * StreamEnvelope is the gateway's per-line wrapper for a streaming RPC: each
 * NDJSON line is {result?, error?}. grpc-gateway emits a trailing line with
 * `error` set when the stream fails mid-flight.
 */
export interface StreamEnvelope<T> {
  result?: T;
  error?: ApiError;
}

/** ValidateWorkflowResponse lists every canonical validation error found. */
export interface ValidateWorkflowResponse {
  valid: boolean;
  errors?: ApiError[];
}

/**
 * SaveWorkflowResponse reports the result of a builder save. `priorHash` and
 * `created` are optional because the gateway omits zero-valued proto fields:
 * a first save carries no `priorHash`, and an overwrite reports `created`
 * absent rather than false.
 */
export interface SaveWorkflowResponse {
  workflowName: string;
  hash: string;
  priorHash?: string;
  created?: boolean;
}

/** MemoryEntry is one curated note. */
export interface MemoryEntry {
  id?: string;
  scope?: string;
  content: string;
  importance?: number;
  tags?: string[];
  tokenCost?: number;
  createdAt?: string;
  updatedAt?: string;
}

export interface SearchMemoryResponse {
  entries?: MemoryEntry[];
  fallback?: boolean;
}

/** Savings is an estimated improvement with a confidence band. */
export interface Savings {
  costUsdPerRun?: number;
  latencyDeltaMs?: string;
  confidence?: string;
}

/** Suggestion is one evidence-backed tuning proposal awaiting review. */
export interface Suggestion {
  id: string;
  kind: string;
  target: string;
  status: string;
  rationale?: string;
  evidence?: JsonObject;
  proposed?: JsonObject;
  estSavings?: Savings;
  createdAt?: string;
}

export interface ListSuggestionsResponse {
  suggestions: Suggestion[];
}

export interface ApproveSuggestionResponse {
  suggestion: Suggestion;
  /** annotation is the rendered change for the operator to apply by hand. */
  annotation?: string;
}

export interface RejectSuggestionResponse {
  suggestion: Suggestion;
}

// ---------------------------------------------------------------------------
// ChatService (D9 conversational surface).
//
// SendMessage and ConfirmAction stream ChatEvents as NDJSON, one
// {result|error} StreamEnvelope per line, the same wire shape as RunWorkflow.
// The unary calls (CreateSession, ListSessions, ListMessages) return the JSON
// bodies below. Note the gateway's proto3 JSON mapping: the SendMessage body
// field is `message` and the session id lives in the URL path, not the body.
// ---------------------------------------------------------------------------

/** ChatSession is one persisted conversation. */
export interface ChatSession {
  id: string;
  /** personality is the session's persona; empty means the server default. */
  personality?: string;
  createdAt?: string;
  updatedAt?: string;
}

/** ChatMessage is one stored conversation entry. role is user, assistant, or
 * tool; the server assigns seq in send order. */
export interface ChatMessage {
  seq?: number;
  role: string;
  content: string;
  createdAt?: string;
}

export interface CreateSessionResponse {
  session: ChatSession;
}

export interface ListSessionsResponse {
  sessions?: ChatSession[];
}

export interface GetSessionResponse {
  session: ChatSession;
}

export interface ListMessagesResponse {
  messages?: ChatMessage[];
}

/**
 * WorkflowProposal is the D9 confirmation payload: the workflow the agent
 * wants to run, its inputs, and the estimated spend. It rides on a
 * CONFIRMATION_REQUIRED event so the user approves from full information. A
 * proposal never runs on its own; the user must confirm through ConfirmAction.
 */
export interface WorkflowProposal {
  workflowName: string;
  inputs?: JsonObject;
  estimatedBudgetUsd?: number;
}

/** ChatEventType says what a ChatEvent describes. The values are the proto
 * enum names the gateway emits, matching the RunEventType convention. */
export type ChatEventType =
  | 'CHAT_EVENT_TYPE_UNSPECIFIED'
  | 'CHAT_EVENT_TYPE_ASSISTANT_CHUNK'
  | 'CHAT_EVENT_TYPE_TOOL_CALL'
  | 'CHAT_EVENT_TYPE_CONFIRMATION_REQUIRED'
  | 'CHAT_EVENT_TYPE_RUN_STARTED'
  | 'CHAT_EVENT_TYPE_RUN_FINISHED'
  | 'CHAT_EVENT_TYPE_TURN_DONE';

/**
 * ChatEvent is one entry in a turn's NDJSON stream. Only the fields a given
 * type uses are set: `text` on ASSISTANT_CHUNK and TURN_DONE, `tool` and
 * `toolInput` on TOOL_CALL, `proposal` on CONFIRMATION_REQUIRED, and
 * `runId`/`runStatus` on RUN_STARTED and RUN_FINISHED.
 */
export interface ChatEvent {
  type: ChatEventType;
  text?: string;
  tool?: string;
  toolInput?: JsonObject;
  proposal?: WorkflowProposal;
  runId?: string;
  runStatus?: string;
}
