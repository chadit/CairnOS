import { describe, expect, it } from 'vitest';

import { type Workflow, emptyAction, emptyWorkflow } from '@/workflow/model';
import { buildTestWorkflow } from './testAction';

function twoActionFlow(): Workflow {
  return {
    ...emptyWorkflow('demo'),
    inputs: {
      topic: { type: 'string', required: true, hasDefault: false, description: 'the subject' },
    },
    actions: {
      fetch: { ...emptyAction('fetch') },
      score: {
        ...emptyAction('score'),
        dependsOn: ['fetch'],
        runner: 'mock',
        params: { output: 'hi' },
      },
    },
  };
}

describe('buildTestWorkflow', () => {
  it('keeps the name, input declarations, and the selected action, stripping depends_on', () => {
    const flow = twoActionFlow();

    const test = buildTestWorkflow(flow, 'score');

    expect(test.name).toBe('demo');
    expect(test.inputs).toEqual(flow.inputs);
    expect(Object.keys(test.actions)).toEqual(['score']);
    expect(test.actions.score?.dependsOn).toEqual([]);
    expect(test.actions.score?.params).toEqual({ output: 'hi' });
    expect(test.actions.score?.runner).toBe('mock');
  });

  it('does not mutate its argument', () => {
    const flow = twoActionFlow();
    const before = structuredClone(flow);

    buildTestWorkflow(flow, 'score');

    expect(flow).toEqual(before);
  });

  it('throws when the named action is absent', () => {
    const flow = twoActionFlow();

    expect(() => buildTestWorkflow(flow, 'missing')).toThrow('missing');
  });
});
