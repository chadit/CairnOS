// Builder state: the canvas graph plus the workflow metadata the canvas does
// not hold (name, inputs, schedule). Server state (runs, runners, suggestions)
// lives in TanStack Query, not here; this store is only the in-progress edit.

import type { Edge } from 'reactflow';
import { create } from 'zustand';

import { type ActionNode, graphToWorkflow, workflowToGraph } from '@/workflow/graph';
import {
  type Action,
  type InputDecl,
  type Workflow,
  emptyAction,
  emptyWorkflow,
} from '@/workflow/model';

// Patch allows an explicit undefined for any field, which a plain Partial
// rejects under exactOptionalPropertyTypes. Inspector inputs clear a field by
// passing undefined, so the patch type has to accept it.
type Patch<T> = { [K in keyof T]?: T[K] | undefined };

type MetaPatch = Patch<
  Pick<Workflow, 'name' | 'version' | 'description' | 'schedule' | 'onError' | 'notify' | 'timeout'>
>;

interface BuilderState {
  /** meta carries workflow fields that are not per-node (name, inputs, etc.). */
  meta: Workflow;
  nodes: ActionNode[];
  edges: Edge[];
  selectedId: string | null;

  loadWorkflow: (flow: Workflow) => void;
  currentWorkflow: () => Workflow;

  /** setNodes and setEdges receive the next graph from the canvas after
   * reactflow has applied its changes. Keeping the reactflow change helpers in
   * the Canvas component keeps reactflow out of this store's import graph, so a
   * page that only reads the graph (the runner view) does not pull it in. */
  setNodes: (nodes: ActionNode[]) => void;
  setEdges: (edges: Edge[]) => void;
  connect: (source: string, target: string) => void;

  addAction: (name: string, kind: Action['kind']) => void;
  updateAction: (name: string, patch: Patch<Action>) => void;
  removeAction: (name: string) => void;
  select: (id: string | null) => void;

  setMeta: (patch: MetaPatch) => void;
  setInput: (name: string, decl: InputDecl) => void;
  removeInput: (name: string) => void;

  setErrorCodes: (byAction: Record<string, string[]>) => void;
}

export const useBuilderStore = create<BuilderState>((set, get) => ({
  meta: emptyWorkflow(),
  nodes: [],
  edges: [],
  selectedId: null,

  loadWorkflow: (flow) => {
    const { nodes, edges } = workflowToGraph(flow);
    set({ meta: flow, nodes, edges, selectedId: null });
  },

  currentWorkflow: () => {
    const { meta, nodes, edges } = get();
    return graphToWorkflow(meta, nodes, edges);
  },

  setNodes: (nodes) => set({ nodes }),

  setEdges: (edges) => set({ edges }),

  connect: (source, target) => {
    if (!source || !target || source === target) {
      return;
    }
    const id = `${source}->${target}`;
    // A stable id keyed on the endpoints keeps depends_on from gaining the same
    // dependency twice when the user reconnects the same pair.
    if (get().edges.some((edge) => edge.id === id)) {
      return;
    }
    set({ edges: [...get().edges, { id, source, target }] });
  },

  addAction: (name, kind) => {
    if (get().nodes.some((node) => node.id === name)) {
      return;
    }
    const action = { ...emptyAction(name), kind };
    const index = get().nodes.length;
    const node: ActionNode = {
      id: name,
      type: 'action',
      position: { x: (index % 3) * 260 + 40, y: Math.floor(index / 3) * 160 + 40 },
      data: { action, errorCodes: [] },
    };
    set({ nodes: [...get().nodes, node], selectedId: name });
  },

  updateAction: (name, patch) => {
    set({
      nodes: get().nodes.map((node) =>
        node.id === name
          ? {
              ...node,
              // The merged spread widens required fields to T | undefined; the
              // assertion drops that back to Action. A patch never blanks a
              // required field, and clearing an optional one with undefined is
              // normalized to absent on emit.
              data: { ...node.data, action: { ...node.data.action, ...patch } as Action },
            }
          : node,
      ),
    });
  },

  removeAction: (name) => {
    set({
      nodes: get().nodes.filter((node) => node.id !== name),
      edges: get().edges.filter((edge) => edge.source !== name && edge.target !== name),
      selectedId: get().selectedId === name ? null : get().selectedId,
    });
  },

  select: (id) => set({ selectedId: id }),

  setMeta: (patch) => set({ meta: { ...get().meta, ...patch } as Workflow }),

  setInput: (name, decl) => {
    set({ meta: { ...get().meta, inputs: { ...get().meta.inputs, [name]: decl } } });
  },

  removeInput: (name) => {
    const inputs = { ...get().meta.inputs };
    delete inputs[name];
    set({ meta: { ...get().meta, inputs } });
  },

  setErrorCodes: (byAction) => {
    set({
      nodes: get().nodes.map((node) => ({
        ...node,
        data: { ...node.data, errorCodes: byAction[node.id] ?? [] },
      })),
    });
  },
}));
