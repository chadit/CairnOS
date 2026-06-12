// Workflow <-> graph mapping. The builder's canvas is reactflow nodes and
// edges; this maps the Workflow model onto that and back. A node is an action,
// an edge is a depends_on relationship pointing from the upstream action to
// the downstream one. The edge direction matches the data flow: source
// produces, target consumes (.agents/spec/02-workflow-schema.md).
//
// Round-trip note: graphToWorkflow rebuilds depends_on purely from edges, so
// the canvas is the single source of truth for the DAG. The mapper carries the
// rest of each action (params, agent settings) untouched on the node data.

import type { Edge, Node } from 'reactflow';

import type { Action, Workflow } from './model';

/** ActionNodeData is what each canvas node carries: the full action plus a
 * place for validation errors keyed by action name. */
export interface ActionNodeData extends Record<string, unknown> {
  action: Action;
  errorCodes: string[];
}

export type ActionNode = Node<ActionNodeData>;

/** layoutGrid places nodes in a simple grid so an imported file is readable
 * before the user drags anything. It is a starting layout, not a real DAG
 * layout; position never affects behavior or the round-trip. */
function layoutGrid(index: number): { x: number; y: number } {
  const perRow = 3;
  const colWidth = 260;
  const rowHeight = 160;
  return {
    x: (index % perRow) * colWidth + 40,
    y: Math.floor(index / perRow) * rowHeight + 40,
  };
}

/** workflowToGraph builds canvas nodes and edges from a workflow. */
export function workflowToGraph(flow: Workflow): { nodes: ActionNode[]; edges: Edge[] } {
  const names = Object.keys(flow.actions).sort();

  const nodes: ActionNode[] = names.map((name, index) => ({
    id: name,
    type: 'action',
    position: layoutGrid(index),
    data: { action: flow.actions[name]!, errorCodes: [] },
  }));

  const edges: Edge[] = [];
  for (const name of names) {
    const action = flow.actions[name]!;
    for (const upstream of action.dependsOn) {
      // Only emit an edge for a dependency whose target action exists; a
      // dangling depends_on is a validation problem the server reports, not
      // something the canvas should crash on.
      if (flow.actions[upstream]) {
        edges.push({
          id: `${upstream}->${name}`,
          source: upstream,
          target: name,
        });
      }
    }
  }

  return { nodes, edges };
}

/**
 * graphToWorkflow rebuilds a workflow from canvas state. The DAG comes from
 * edges, so depends_on on the node data is ignored and recomputed. Everything
 * else on each action is preserved. The base workflow supplies the metadata
 * (name, inputs, schedule) the canvas does not hold.
 */
export function graphToWorkflow(base: Workflow, nodes: ActionNode[], edges: Edge[]): Workflow {
  const incoming = new Map<string, Set<string>>();
  for (const node of nodes) {
    incoming.set(node.id, new Set());
  }
  for (const edge of edges) {
    const deps = incoming.get(edge.target);
    // An edge to or from a node that no longer exists is dropped rather than
    // recorded; the canvas should never persist a half-deleted edge.
    if (deps && incoming.has(edge.source)) {
      deps.add(edge.source);
    }
  }

  const actions: Record<string, Action> = {};
  for (const node of nodes) {
    const deps = incoming.get(node.id) ?? new Set<string>();
    actions[node.id] = {
      ...node.data.action,
      name: node.id,
      dependsOn: [...deps].sort(),
    };
  }

  return { ...base, actions };
}
