// Semantic normalization. Two workflows are behaviorally equal when their
// normalized forms deep-equal. Normalizing collapses the things that do not
// change behavior: stamped defaults (version "1", on_error "halt", kind
// "task"), the order of depends_on and tools entries, and the order of action
// and input keys. What stays is the actions, the edges, the params, and the
// agent settings, which is exactly the round-trip contract
// (.agents/spec/12-ui-and-governance.md, acceptance criteria).

import type { JsonValue } from '@/api/types';
import {
  type Action,
  type InputDecl,
  type RetryPolicy,
  type Workflow,
  DEFAULT_KIND,
  DEFAULT_ON_ERROR,
  DEFAULT_VERSION,
} from './model';

/** A normalized, comparison-ready workflow. Field presence is significant. */
export interface NormalWorkflow {
  name: string;
  version: string;
  description: string;
  schedule: string;
  onError: string;
  notify: string[];
  timeout: string;
  inputs: Record<string, NormalInput>;
  actions: Record<string, NormalAction>;
}

interface NormalInput {
  type: string;
  required: boolean;
  default: JsonValue | undefined;
  description: string;
}

interface NormalAction {
  kind: string;
  dependsOn: string[];
  idempotent: boolean;
  timeout: string;
  condition: string;
  retry: RetryPolicy | undefined;
  runner: string;
  params: Record<string, JsonValue>;
  driver: string;
  goal: string;
  tools: string[] | undefined;
  maxIterations: number;
  budgetUsd: number;
  systemPrompt: string;
  memoryScope: string;
}

/** normalizeWorkflow produces the canonical comparison form of a workflow. */
export function normalizeWorkflow(flow: Workflow): NormalWorkflow {
  return {
    name: flow.name,
    version: flow.version || DEFAULT_VERSION,
    description: flow.description,
    schedule: flow.schedule ?? '',
    onError: flow.onError || DEFAULT_ON_ERROR,
    notify: [...flow.notify].sort(),
    timeout: flow.timeout ?? '',
    inputs: sortRecord(flow.inputs, normalizeInput),
    actions: sortRecord(flow.actions, normalizeAction),
  };
}

function normalizeInput(decl: InputDecl): NormalInput {
  return {
    type: decl.type,
    required: decl.required,
    default: decl.hasDefault ? decl.default : undefined,
    description: decl.description,
  };
}

function normalizeAction(action: Action): NormalAction {
  return {
    kind: action.kind || DEFAULT_KIND,
    // depends_on order never changes behavior: the DAG is a set of edges.
    dependsOn: [...action.dependsOn].sort(),
    idempotent: action.idempotent,
    timeout: action.timeout ?? '',
    condition: action.condition ?? '',
    retry: action.retry ? normalizeRetry(action.retry) : undefined,
    runner: action.runner ?? '',
    params: normalizeJsonObject(action.params ?? {}),
    driver: action.driver ?? '',
    goal: action.goal ?? '',
    // A declared empty tools list ([]) is distinct from never declaring tools,
    // so an undeclared list stays undefined and an empty one stays [].
    tools: action.hasTools ? [...(action.tools ?? [])].sort() : undefined,
    maxIterations: action.maxIterations ?? 0,
    budgetUsd: action.budgetUsd ?? 0,
    systemPrompt: action.systemPrompt ?? '',
    memoryScope: action.memoryScope ?? '',
  };
}

function normalizeRetry(retry: RetryPolicy): RetryPolicy {
  return {
    maxAttempts: retry.maxAttempts ?? 0,
    backoff: retry.backoff ?? '',
    baseDelay: retry.baseDelay ?? '',
    maxDelay: retry.maxDelay ?? '',
    jitter: retry.jitter ?? false,
  };
}

/** normalizeJsonObject sorts a params map's keys so comparison is order-free. */
function normalizeJsonObject(obj: Record<string, JsonValue>): Record<string, JsonValue> {
  const out: Record<string, JsonValue> = {};
  for (const key of Object.keys(obj).sort()) {
    out[key] = normalizeJsonValue(obj[key]!);
  }
  return out;
}

function normalizeJsonValue(value: JsonValue): JsonValue {
  if (Array.isArray(value)) {
    // Array element order in params can be meaningful, so it is preserved.
    return value.map(normalizeJsonValue);
  }
  if (value !== null && typeof value === 'object') {
    return normalizeJsonObject(value);
  }
  return value;
}

function sortRecord<T, R>(record: Record<string, T>, fn: (value: T) => R): Record<string, R> {
  const out: Record<string, R> = {};
  for (const key of Object.keys(record).sort()) {
    out[key] = fn(record[key]!);
  }
  return out;
}
