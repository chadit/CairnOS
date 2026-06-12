// TOML text to Workflow model. This mirrors internal/workflow/parse.go's
// build/buildMeta/buildAction so the UI reads a file the same way the engine
// does: same defaults stamped, same reserved keys rejected. It does NOT
// re-run the engine's deep validation (cycles, reference-vs-depends_on); that
// is the server's job via ValidateWorkflow. Parse here only maps structure.

import { parse as parseToml } from 'smol-toml';

import type { JsonValue } from '@/api/types';
import {
  type Action,
  type ActionKind,
  type InputDecl,
  type InputType,
  type RetryPolicy,
  type Workflow,
  DEFAULT_KIND,
  DEFAULT_ON_ERROR,
  DEFAULT_VERSION,
} from './model';

/** ParseError carries a human message and the key that triggered it. */
export class WorkflowParseError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'WorkflowParseError';
  }
}

const RESERVED_WORKFLOW_KEYS = new Set(['budget', 'imports']);
const RESERVED_ACTION_KEYS = new Set(['foreach', 'budget']);

/**
 * parseWorkflow turns TOML bytes into the Workflow model. It throws a
 * WorkflowParseError on a structural problem (bad TOML, reserved key, wrong
 * type) so the caller surfaces one clear message rather than a partial graph.
 */
export function parseWorkflow(toml: string): Workflow {
  let raw: unknown;
  try {
    raw = parseToml(toml);
  } catch (err) {
    const detail = err instanceof Error ? err.message : String(err);
    throw new WorkflowParseError(`not valid TOML: ${detail}`);
  }

  if (!isTable(raw)) {
    throw new WorkflowParseError('workflow file must be a TOML table');
  }

  for (const key of Object.keys(raw)) {
    if (key !== 'workflow' && key !== 'action') {
      throw new WorkflowParseError(`unknown top-level table "${key}"`);
    }
  }

  const meta = raw.workflow;
  if (!isTable(meta)) {
    throw new WorkflowParseError('missing [workflow] table');
  }

  const flow: Workflow = {
    name: '',
    version: DEFAULT_VERSION,
    description: '',
    onError: DEFAULT_ON_ERROR,
    notify: [],
    inputs: {},
    actions: {},
  };

  buildMeta(flow, meta);

  const actionsRaw = raw.action;
  if (actionsRaw !== undefined) {
    if (!isTable(actionsRaw)) {
      throw new WorkflowParseError('[action] must be a table of tables');
    }
    for (const [name, body] of Object.entries(actionsRaw)) {
      if (!isTable(body)) {
        throw new WorkflowParseError(`action "${name}" must be a table`);
      }
      flow.actions[name] = buildAction(name, body);
    }
  }

  return flow;
}

function buildMeta(flow: Workflow, meta: Record<string, unknown>): void {
  for (const [key, value] of Object.entries(meta)) {
    switch (key) {
      case 'name':
        flow.name = asString('workflow.name', value);
        break;
      case 'version':
        flow.version = asString('workflow.version', value);
        break;
      case 'description':
        flow.description = asString('workflow.description', value);
        break;
      case 'schedule':
        flow.schedule = asString('workflow.schedule', value);
        break;
      case 'on_error':
        flow.onError = buildOnError(value);
        break;
      case 'notify':
        flow.notify = asStringArray('workflow.notify', value);
        break;
      case 'timeout':
        flow.timeout = asString('workflow.timeout', value);
        break;
      case 'inputs':
        flow.inputs = buildInputs(value);
        break;
      default:
        if (RESERVED_WORKFLOW_KEYS.has(key)) {
          throw new WorkflowParseError(`workflow key "${key}" is reserved for a future phase`);
        }
        throw new WorkflowParseError(`unknown workflow key "${key}"`);
    }
  }
}

function buildOnError(value: unknown): string {
  const policy = asString('workflow.on_error', value);
  if (policy === 'halt' || policy === 'continue') {
    return policy;
  }
  if (policy === 'rollback') {
    throw new WorkflowParseError('on_error = "rollback" is reserved for a future phase');
  }
  throw new WorkflowParseError(`on_error must be "halt" or "continue", got "${policy}"`);
}

function buildInputs(value: unknown): Record<string, InputDecl> {
  if (!isTable(value)) {
    throw new WorkflowParseError('workflow.inputs must be a table');
  }

  const inputs: Record<string, InputDecl> = {};
  for (const [name, declRaw] of Object.entries(value)) {
    if (!isTable(declRaw)) {
      throw new WorkflowParseError(`input "${name}" must be a table`);
    }
    inputs[name] = buildInputDecl(name, declRaw);
  }
  return inputs;
}

function buildInputDecl(name: string, decl: Record<string, unknown>): InputDecl {
  const out: InputDecl = { type: 'string', required: false, hasDefault: false, description: '' };

  for (const [key, value] of Object.entries(decl)) {
    switch (key) {
      case 'type':
        out.type = asInputType(name, value);
        break;
      case 'required':
        out.required = asBool(`input ${name} required`, value);
        break;
      case 'default':
        out.default = value as JsonValue;
        out.hasDefault = true;
        break;
      case 'description':
        out.description = asString(`input ${name} description`, value);
        break;
      default:
        throw new WorkflowParseError(`input "${name}" has unknown key "${key}"`);
    }
  }

  return out;
}

function buildAction(name: string, table: Record<string, unknown>): Action {
  const action: Action = {
    name,
    kind: DEFAULT_KIND,
    dependsOn: [],
    idempotent: false,
    hasTools: false,
  };

  for (const [key, value] of Object.entries(table)) {
    switch (key) {
      case 'kind':
        action.kind = asKind(name, value);
        break;
      case 'depends_on':
        action.dependsOn = asStringArray(`${name}.depends_on`, value);
        break;
      case 'idempotent':
        action.idempotent = asBool(`${name}.idempotent`, value);
        break;
      case 'timeout':
        action.timeout = asString(`${name}.timeout`, value);
        break;
      case 'condition':
        action.condition = asString(`${name}.condition`, value);
        break;
      case 'retry':
        action.retry = buildRetry(name, value);
        break;
      case 'runner':
        action.runner = asString(`${name}.runner`, value);
        break;
      case 'params':
        action.params = asTable(`${name}.params`, value);
        break;
      case 'driver':
        action.driver = asString(`${name}.driver`, value);
        break;
      case 'goal':
        action.goal = asString(`${name}.goal`, value);
        break;
      case 'tools':
        action.tools = asStringArray(`${name}.tools`, value);
        action.hasTools = true;
        break;
      case 'max_iterations':
        action.maxIterations = asInt(`${name}.max_iterations`, value);
        break;
      case 'budget_usd':
        action.budgetUsd = asNumber(`${name}.budget_usd`, value);
        break;
      case 'system_prompt':
        action.systemPrompt = asString(`${name}.system_prompt`, value);
        break;
      case 'memory_scope':
        action.memoryScope = asString(`${name}.memory_scope`, value);
        break;
      default:
        if (RESERVED_ACTION_KEYS.has(key)) {
          throw new WorkflowParseError(
            `action "${name}" key "${key}" is reserved for a future phase`,
          );
        }
        throw new WorkflowParseError(`action "${name}" has unknown key "${key}"`);
    }
  }

  return action;
}

function buildRetry(name: string, value: unknown): RetryPolicy {
  if (!isTable(value)) {
    throw new WorkflowParseError(`action "${name}" retry must be a table`);
  }

  const policy: RetryPolicy = {};
  for (const [key, fieldValue] of Object.entries(value)) {
    switch (key) {
      case 'max_attempts':
        policy.maxAttempts = asInt(`${name}.retry.max_attempts`, fieldValue);
        break;
      case 'backoff':
        policy.backoff = asString(`${name}.retry.backoff`, fieldValue);
        break;
      case 'base_delay':
        policy.baseDelay = asString(`${name}.retry.base_delay`, fieldValue);
        break;
      case 'max_delay':
        policy.maxDelay = asString(`${name}.retry.max_delay`, fieldValue);
        break;
      case 'jitter':
        policy.jitter = asBool(`${name}.retry.jitter`, fieldValue);
        break;
      default:
        throw new WorkflowParseError(`action "${name}" retry has unknown key "${key}"`);
    }
  }

  return policy;
}

// Typed accessors. Each throws on a type mismatch with the offending key
// named, the way the engine's builder reports a CodeInvalid error.

function isTable(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function asString(field: string, value: unknown): string {
  if (typeof value !== 'string') {
    throw new WorkflowParseError(`${field} must be a string`);
  }
  return value;
}

function asBool(field: string, value: unknown): boolean {
  if (typeof value !== 'boolean') {
    throw new WorkflowParseError(`${field} must be a bool`);
  }
  return value;
}

function asInt(field: string, value: unknown): number {
  if (typeof value !== 'number' || !Number.isInteger(value)) {
    throw new WorkflowParseError(`${field} must be an integer`);
  }
  return value;
}

function asNumber(field: string, value: unknown): number {
  if (typeof value !== 'number') {
    throw new WorkflowParseError(`${field} must be a number`);
  }
  return value;
}

function asStringArray(field: string, value: unknown): string[] {
  if (!Array.isArray(value)) {
    throw new WorkflowParseError(`${field} must be an array of strings`);
  }
  return value.map((item) => {
    if (typeof item !== 'string') {
      throw new WorkflowParseError(`${field} must contain only strings`);
    }
    return item;
  });
}

function asTable(field: string, value: unknown): Record<string, JsonValue> {
  if (!isTable(value)) {
    throw new WorkflowParseError(`${field} must be a table`);
  }
  return value as Record<string, JsonValue>;
}

function asKind(name: string, value: unknown): ActionKind {
  const kind = asString(`${name}.kind`, value);
  if (kind === 'task' || kind === 'agent') {
    return kind;
  }
  throw new WorkflowParseError(`action "${name}" kind must be "task" or "agent", got "${kind}"`);
}

function asInputType(name: string, value: unknown): InputType {
  const type = asString(`input ${name} type`, value);
  if (
    type === 'string' ||
    type === 'int' ||
    type === 'float' ||
    type === 'bool' ||
    type === 'string[]'
  ) {
    return type;
  }
  throw new WorkflowParseError(`input "${name}" has unknown type "${type}"`);
}
