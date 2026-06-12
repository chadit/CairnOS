// Workflow model to TOML text. The emitter writes a canonical form: tables in
// a fixed order, only non-default fields present, so the output is stable and
// re-parses to the same Workflow (.agents/spec/12-ui-and-governance.md,
// "semantic round-trip"). Comments and the author's original formatting are
// not reproduced; behavior is.
//
// We build TOML by hand rather than handing the model to a generic encoder so
// the inline-vs-block table choices (params as an inline table, retry as a
// sub-table) match what the engine examples use and what re-parses cleanly.

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

/** emitWorkflow renders a Workflow as canonical TOML text. */
export function emitWorkflow(flow: Workflow): string {
  const blocks: string[] = [];

  blocks.push(emitMeta(flow));

  for (const name of Object.keys(flow.inputs).sort()) {
    blocks.push(emitInput(name, flow.inputs[name]!));
  }

  for (const name of Object.keys(flow.actions).sort()) {
    blocks.push(emitAction(flow.actions[name]!));
  }

  return blocks.join('\n') + '\n';
}

function emitMeta(flow: Workflow): string {
  const lines = ['[workflow]', `name = ${tomlString(flow.name)}`];

  if (flow.version && flow.version !== DEFAULT_VERSION) {
    lines.push(`version = ${tomlString(flow.version)}`);
  }
  if (flow.description) {
    lines.push(`description = ${tomlString(flow.description)}`);
  }
  if (flow.schedule) {
    lines.push(`schedule = ${tomlString(flow.schedule)}`);
  }
  if (flow.onError && flow.onError !== DEFAULT_ON_ERROR) {
    lines.push(`on_error = ${tomlString(flow.onError)}`);
  }
  if (flow.notify.length > 0) {
    lines.push(`notify = ${tomlArray(flow.notify)}`);
  }
  if (flow.timeout) {
    lines.push(`timeout = ${tomlString(flow.timeout)}`);
  }

  return lines.join('\n') + '\n';
}

function emitInput(name: string, decl: InputDecl): string {
  const lines = [`[workflow.inputs.${name}]`, `type = ${tomlString(decl.type)}`];

  if (decl.required) {
    lines.push('required = true');
  }
  if (decl.hasDefault && decl.default !== undefined) {
    lines.push(`default = ${tomlValue(decl.default)}`);
  }
  if (decl.description) {
    lines.push(`description = ${tomlString(decl.description)}`);
  }

  return lines.join('\n') + '\n';
}

function emitAction(action: Action): string {
  const lines = [`[action.${action.name}]`];

  if (action.kind !== DEFAULT_KIND) {
    lines.push(`kind = ${tomlString(action.kind)}`);
  }
  if (action.dependsOn.length > 0) {
    lines.push(`depends_on = ${tomlArray(action.dependsOn)}`);
  }
  if (action.idempotent) {
    lines.push('idempotent = true');
  }
  if (action.timeout) {
    lines.push(`timeout = ${tomlString(action.timeout)}`);
  }
  if (action.condition) {
    lines.push(`condition = ${tomlString(action.condition)}`);
  }

  if (action.kind === 'task') {
    if (action.runner) {
      lines.push(`runner = ${tomlString(action.runner)}`);
    }
    if (action.params && Object.keys(action.params).length > 0) {
      lines.push(`params = ${tomlInlineTable(action.params)}`);
    }
  } else {
    if (action.driver) {
      lines.push(`driver = ${tomlString(action.driver)}`);
    }
    if (action.goal) {
      lines.push(`goal = ${tomlString(action.goal)}`);
    }
    if (action.hasTools && action.tools) {
      lines.push(`tools = ${tomlArray(action.tools)}`);
    }
    if (action.maxIterations !== undefined) {
      lines.push(`max_iterations = ${String(action.maxIterations)}`);
    }
    if (action.budgetUsd !== undefined) {
      lines.push(`budget_usd = ${tomlNumber(action.budgetUsd)}`);
    }
    if (action.systemPrompt) {
      lines.push(`system_prompt = ${tomlString(action.systemPrompt)}`);
    }
    if (action.memoryScope) {
      lines.push(`memory_scope = ${tomlString(action.memoryScope)}`);
    }
  }

  let block = lines.join('\n') + '\n';

  if (action.retry) {
    block += '\n' + emitRetry(action.name, action.retry);
  }

  return block;
}

function emitRetry(actionName: string, retry: RetryPolicy): string {
  const lines = [`[action.${actionName}.retry]`];

  if (retry.maxAttempts !== undefined) {
    lines.push(`max_attempts = ${String(retry.maxAttempts)}`);
  }
  if (retry.backoff) {
    lines.push(`backoff = ${tomlString(retry.backoff)}`);
  }
  if (retry.baseDelay) {
    lines.push(`base_delay = ${tomlString(retry.baseDelay)}`);
  }
  if (retry.maxDelay) {
    lines.push(`max_delay = ${tomlString(retry.maxDelay)}`);
  }
  if (retry.jitter !== undefined) {
    lines.push(`jitter = ${String(retry.jitter)}`);
  }

  return lines.join('\n') + '\n';
}

// TOML scalar and collection encoders. Strings always use the basic
// double-quoted form with escapes, which carries newlines and quotes
// faithfully so a multi-line prompt round-trips without depending on the
// triple-quoted literal form.

function tomlString(value: string): string {
  const escaped = value
    .replace(/\\/g, '\\\\')
    .replace(/"/g, '\\"')
    .replace(/\n/g, '\\n')
    .replace(/\r/g, '\\r')
    .replace(/\t/g, '\\t');
  return `"${escaped}"`;
}

function tomlNumber(value: number): string {
  // A whole-valued float must keep a decimal point so it re-parses as a float,
  // matching budget_usd's number type rather than an integer.
  if (Number.isInteger(value)) {
    return `${String(value)}.0`;
  }
  return String(value);
}

function tomlArray(items: string[]): string {
  return `[${items.map(tomlString).join(', ')}]`;
}

function tomlInlineTable(table: Record<string, JsonValue>): string {
  const parts = Object.keys(table)
    .sort()
    .map((key) => `${tomlKey(key)} = ${tomlValue(table[key]!)}`);
  return `{ ${parts.join(', ')} }`;
}

function tomlKey(key: string): string {
  // A bare key is allowed only for the [A-Za-z0-9_-] set; anything else needs
  // quoting so the emitted TOML parses.
  return /^[A-Za-z0-9_-]+$/.test(key) ? key : tomlString(key);
}

function tomlValue(value: JsonValue): string {
  if (typeof value === 'string') {
    return tomlString(value);
  }
  if (typeof value === 'number') {
    return String(value);
  }
  if (typeof value === 'boolean') {
    return String(value);
  }
  if (value === null) {
    // TOML has no null; the engine never emits one, and the builder never
    // produces one. Encode as an empty string so the output stays valid TOML.
    return '""';
  }
  if (Array.isArray(value)) {
    return `[${value.map(tomlValue).join(', ')}]`;
  }
  return tomlInlineTable(value);
}
