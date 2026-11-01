// The workflow model, a TypeScript mirror of internal/workflow/workflow.go.
// This is the canonical in-memory shape the builder edits and the TOML mapper
// reads and writes. Field names and defaults match the Go parser exactly so a
// round-trip stays faithful.

import type { JsonValue } from '@/api/types';

/** ActionKind selects between a task action and an agent loop. */
export type ActionKind = 'task' | 'agent';

/** Default version the Go parser stamps when `version` is absent. */
export const DEFAULT_VERSION = '1';

/** Default on_error policy the Go parser stamps when `on_error` is absent. */
export const DEFAULT_ON_ERROR = 'halt';

/** Default action kind when `kind` is absent. */
export const DEFAULT_KIND: ActionKind = 'task';

/** Declared input types accepted by the schema. */
export type InputType = 'string' | 'int' | 'float' | 'bool' | 'string[]';

/** InputDecl is one declared run input. */
export interface InputDecl {
  type: InputType;
  required: boolean;
  /** default is present only when the file declared one. */
  default?: JsonValue;
  hasDefault: boolean;
  description: string;
}

/** RetryPolicy mirrors a per-action [retry] table. */
export interface RetryPolicy {
  maxAttempts?: number;
  backoff?: string;
  baseDelay?: string;
  maxDelay?: string;
  jitter?: boolean;
}

/**
 * Action is one unit of work. Task actions carry runner/params; agent actions
 * carry driver/goal/tools/budget. Both share the dependency and control
 * fields. Durations stay as their original strings ("30m", "2s") so the
 * round-trip emits exactly what it read.
 */
export interface Action {
  name: string;
  kind: ActionKind;
  dependsOn: string[];
  idempotent: boolean;
  timeout?: string;
  condition?: string;
  retry?: RetryPolicy;

  // Task fields.
  runner?: string;
  params?: Record<string, JsonValue>;

  // Agent fields.
  driver?: string;
  goal?: string;
  tools?: string[];
  hasTools: boolean;
  maxIterations?: number;
  budgetUsd?: number;
  systemPrompt?: string;
  memoryScope?: string;
}

/** Workflow is one parsed workflow: a named DAG of actions. */
export interface Workflow {
  name: string;
  version: string;
  description: string;
  schedule?: string;
  onError: string;
  notify: string[];
  timeout?: string;
  inputs: Record<string, InputDecl>;
  actions: Record<string, Action>;
}

/** emptyWorkflow returns a blank workflow with the parser's defaults applied. */
export function emptyWorkflow(name = 'untitled'): Workflow {
  return {
    name,
    version: DEFAULT_VERSION,
    description: '',
    onError: DEFAULT_ON_ERROR,
    notify: [],
    inputs: {},
    actions: {},
  };
}

/** emptyAction returns a blank task action with the parser's defaults. */
export function emptyAction(name: string): Action {
  return {
    name,
    kind: DEFAULT_KIND,
    dependsOn: [],
    idempotent: false,
    hasTools: false,
  };
}
