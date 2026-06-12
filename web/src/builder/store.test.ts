import { beforeEach, describe, expect, it } from 'vitest';

import { emptyWorkflow } from '@/workflow/model';
import { useBuilderStore } from './store';

// The store is the builder's edit state. These tests drive it the way the UI
// does (add an action, connect two, edit a param) and check the resulting
// workflow, which is what export and validate read.

function reset() {
  useBuilderStore.getState().loadWorkflow(emptyWorkflow('test'));
}

describe('builder store', () => {
  beforeEach(reset);

  it('adds a node and reflects it in the current workflow', () => {
    useBuilderStore.getState().addAction('fetch', 'task');

    const flow = useBuilderStore.getState().currentWorkflow();
    expect(Object.keys(flow.actions)).toEqual(['fetch']);
    expect(flow.actions.fetch?.kind).toBe('task');
  });

  it('ignores a duplicate action name', () => {
    useBuilderStore.getState().addAction('fetch', 'task');
    useBuilderStore.getState().addAction('fetch', 'agent');

    expect(useBuilderStore.getState().nodes).toHaveLength(1);
    // The first add wins; the kind is not overwritten by the ignored second.
    expect(useBuilderStore.getState().currentWorkflow().actions.fetch?.kind).toBe('task');
  });

  it('connecting two nodes becomes a depends_on edge', () => {
    const store = useBuilderStore.getState();
    store.addAction('fetch', 'task');
    store.addAction('score', 'task');
    store.connect('fetch', 'score');

    const flow = useBuilderStore.getState().currentWorkflow();
    expect(flow.actions.score?.dependsOn).toEqual(['fetch']);
    expect(flow.actions.fetch?.dependsOn).toEqual([]);
  });

  it('connecting the same pair twice keeps one edge', () => {
    const store = useBuilderStore.getState();
    store.addAction('a', 'task');
    store.addAction('b', 'task');
    store.connect('a', 'b');
    store.connect('a', 'b');

    expect(useBuilderStore.getState().edges).toHaveLength(1);
    expect(useBuilderStore.getState().currentWorkflow().actions.b?.dependsOn).toEqual(['a']);
  });

  it('refuses a self-connecting edge', () => {
    const store = useBuilderStore.getState();
    store.addAction('a', 'task');
    store.connect('a', 'a');

    expect(useBuilderStore.getState().edges).toHaveLength(0);
  });

  it('editing a param updates the action', () => {
    const store = useBuilderStore.getState();
    store.addAction('greet', 'task');
    store.updateAction('greet', { runner: 'mock', params: { output: 'hi' } });

    const action = useBuilderStore.getState().currentWorkflow().actions.greet;
    expect(action?.runner).toBe('mock');
    expect(action?.params).toEqual({ output: 'hi' });
  });

  it('removing an action also drops its edges', () => {
    const store = useBuilderStore.getState();
    store.addAction('a', 'task');
    store.addAction('b', 'task');
    store.connect('a', 'b');
    store.removeAction('a');

    const state = useBuilderStore.getState();
    expect(state.nodes.map((node) => node.id)).toEqual(['b']);
    expect(state.edges).toHaveLength(0);
  });

  it('maps validation error codes onto the matching node', () => {
    const store = useBuilderStore.getState();
    store.addAction('a', 'task');
    store.addAction('b', 'task');
    store.setErrorCodes({ a: ['workflow.bad_reference'] });

    const state = useBuilderStore.getState();
    expect(state.nodes.find((node) => node.id === 'a')?.data.errorCodes).toEqual([
      'workflow.bad_reference',
    ]);
    expect(state.nodes.find((node) => node.id === 'b')?.data.errorCodes).toEqual([]);
  });
});
