import { describe, expect, it } from 'vitest';

import { roundTripFixtures } from '@/test/fixtures';
import { emitWorkflow } from './emit';
import { graphToWorkflow, workflowToGraph } from './graph';
import { normalizeWorkflow } from './normalize';
import { parseWorkflow } from './parse';

// The round-trip contract: a workflow parsed from TOML, then emitted and
// re-parsed, must normalize to the same form. Behavior (actions, edges,
// params, agent settings) survives; comments and formatting do not. This test
// proves that contract mechanically.
describe('TOML semantic round-trip', () => {
  it.each(roundTripFixtures)('preserves behavior for $name', ({ toml }) => {
    const original = parseWorkflow(toml);
    const reparsed = parseWorkflow(emitWorkflow(original));

    expect(normalizeWorkflow(reparsed)).toEqual(normalizeWorkflow(original));
  });

  // The builder edits a graph, not TOML. The full path the UI takes is
  // TOML -> graph -> TOML, so that must round-trip too.
  it.each(roundTripFixtures)('survives the graph mapping for $name', ({ toml }) => {
    const original = parseWorkflow(toml);
    const { nodes, edges } = workflowToGraph(original);
    const fromGraph = graphToWorkflow(original, nodes, edges);
    const emitted = emitWorkflow(fromGraph);
    const reparsed = parseWorkflow(emitted);

    expect(normalizeWorkflow(reparsed)).toEqual(normalizeWorkflow(original));
  });
});

describe('graph mapping fidelity', () => {
  it('builds one edge per depends_on entry', () => {
    const flow = parseWorkflow(roundTripFixtures[2]!.toml); // fan-in
    const { edges } = workflowToGraph(flow);

    // merge depends on fetch_a and fetch_b: two edges into merge.
    const intoMerge = edges.filter((edge) => edge.target === 'merge');
    expect(intoMerge.map((edge) => edge.source).sort()).toEqual(['fetch_a', 'fetch_b']);
  });

  it('rebuilds depends_on from edges, not from stale node data', () => {
    const flow = parseWorkflow(roundTripFixtures[0]!.toml); // hello: greet -> echo
    const { nodes, edges } = workflowToGraph(flow);

    // Drop the greet->echo edge; echo must lose its dependency on rebuild.
    const withoutEdge = edges.filter((edge) => edge.id !== 'greet->echo');
    const rebuilt = graphToWorkflow(flow, nodes, withoutEdge);

    expect(rebuilt.actions.echo?.dependsOn).toEqual([]);
  });
});

describe('parse defaults and rejections', () => {
  it('stamps the parser defaults when omitted', () => {
    const flow = parseWorkflow('[workflow]\nname = "w"\n[action.a]\nrunner = "mock"\n');
    expect(flow.version).toBe('1');
    expect(flow.onError).toBe('halt');
    expect(flow.actions.a?.kind).toBe('task');
  });

  it('rejects a reserved workflow key', () => {
    expect(() =>
      parseWorkflow('[workflow]\nname = "w"\n[workflow.budget]\nlimit_usd = 5\n'),
    ).toThrow(/reserved for a future phase/);
  });

  it('rejects on_error = rollback as reserved', () => {
    expect(() =>
      parseWorkflow('[workflow]\nname = "w"\non_error = "rollback"\n[action.a]\nrunner = "m"\n'),
    ).toThrow(/reserved for a future phase/);
  });

  it('rejects an unknown action key', () => {
    expect(() => parseWorkflow('[workflow]\nname = "w"\n[action.a]\nbogus = 1\n')).toThrow(
      /unknown key "bogus"/,
    );
  });

  it('rejects a file with no [workflow] table', () => {
    expect(() => parseWorkflow('[action.a]\nrunner = "m"\n')).toThrow(/missing \[workflow\] table/);
  });
});

describe('emit fidelity', () => {
  it('keeps a budget_usd as a float so it re-parses as a number', () => {
    const flow = parseWorkflow(roundTripFixtures[1]!.toml); // agent, budget_usd = 0.25
    const emitted = emitWorkflow(flow);
    expect(emitted).toMatch(/budget_usd = 0\.25/);
  });

  it('does not emit a stamped default version', () => {
    const flow = parseWorkflow('[workflow]\nname = "w"\nversion = "1"\n[action.a]\nrunner = "m"\n');
    const emitted = emitWorkflow(flow);
    // version "1" is the default, so the canonical emit omits it.
    expect(emitted).not.toMatch(/version =/);
  });
});
