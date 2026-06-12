import type {
  ApproveSuggestionResponse,
  CancelRunResponse,
  GetRunResponse,
  HealthResponse,
  ListRunnersResponse,
  ListRunsResponse,
  ListSuggestionsResponse,
  LoginResponse,
  RejectSuggestionResponse,
  Run,
  SearchMemoryResponse,
  Suggestion,
  ValidateWorkflowResponse,
} from './types';
import { request } from './client';
import { setToken } from './token';

// One function per gateway RPC, grouped by service. Each returns the typed
// response directly; failures reject with a CairnApiError carrying the
// canonical {code, message}.

/** login exchanges an access key for a token and stores it for later calls. */
export async function login(key: string): Promise<LoginResponse> {
  const res = await request<LoginResponse>('/login', { method: 'POST', body: { key } });
  setToken(res.token);
  return res;
}

/** health reports gateway readiness. Never needs a token. */
export function health(): Promise<HealthResponse> {
  return request<HealthResponse>('/health');
}

/** listRunners lists the registered runner names. */
export function listRunners(): Promise<ListRunnersResponse> {
  return request<ListRunnersResponse>('/runners');
}

interface ListRunsParams {
  workflow?: string | undefined;
  status?: string | undefined;
  limit?: number | undefined;
}

/** listRuns lists runs newest-first, filterable by workflow and status. */
export function listRuns(params: ListRunsParams = {}): Promise<ListRunsResponse> {
  return request<ListRunsResponse>('/runs', {
    query: { workflow: params.workflow, status: params.status, limit: params.limit },
  });
}

/** getRun fetches one run's current state and its action results. */
export function getRun(runId: string): Promise<GetRunResponse> {
  return request<GetRunResponse>(`/runs/${encodeURIComponent(runId)}`);
}

/** cancelRun cancels an in-flight run. */
export function cancelRun(runId: string): Promise<CancelRunResponse> {
  return request<CancelRunResponse>(`/runs/${encodeURIComponent(runId)}/cancel`, {
    method: 'POST',
    body: {},
  });
}

/**
 * validateWorkflow parses and validates raw TOML without running it. The
 * gateway expects the bytes base64-encoded per the proto3 JSON mapping for a
 * bytes field.
 */
export function validateWorkflow(toml: string): Promise<ValidateWorkflowResponse> {
  return request<ValidateWorkflowResponse>('/workflows/validate', {
    method: 'POST',
    body: { toml: base64Encode(toml) },
  });
}

interface SearchMemoryParams {
  scope?: string | undefined;
  text?: string | undefined;
  tags?: string[] | undefined;
  limit?: number | undefined;
  semantic?: boolean | undefined;
}

/** searchMemory returns memory entries relevant to the query. */
export function searchMemory(params: SearchMemoryParams = {}): Promise<SearchMemoryResponse> {
  return request<SearchMemoryResponse>('/memory', {
    query: {
      scope: params.scope,
      text: params.text,
      tags: params.tags,
      limit: params.limit,
      semantic: params.semantic,
    },
  });
}

interface ListSuggestionsParams {
  status?: string | undefined;
  limit?: number | undefined;
}

/** listSuggestions lists tuning suggestions newest-first. */
export function listSuggestions(
  params: ListSuggestionsParams = {},
): Promise<ListSuggestionsResponse> {
  return request<ListSuggestionsResponse>('/suggestions', {
    query: { status: params.status, limit: params.limit },
  });
}

/**
 * approveSuggestion marks a suggestion approved and returns the annotated
 * change for the operator to apply by hand. Nothing is written to any file.
 */
export function approveSuggestion(id: string): Promise<ApproveSuggestionResponse> {
  return request<ApproveSuggestionResponse>(`/suggestions/${encodeURIComponent(id)}/approve`, {
    method: 'POST',
    body: {},
  });
}

/** rejectSuggestion marks a suggestion rejected. */
export function rejectSuggestion(id: string): Promise<RejectSuggestionResponse> {
  return request<RejectSuggestionResponse>(`/suggestions/${encodeURIComponent(id)}/reject`, {
    method: 'POST',
    body: {},
  });
}

/** base64Encode encodes a UTF-8 string for the validate request's bytes field. */
function base64Encode(text: string): string {
  const bytes = new TextEncoder().encode(text);
  let binary = '';
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return btoa(binary);
}

export type { ListRunsParams, ListSuggestionsParams, SearchMemoryParams };
export type { Run, Suggestion };
