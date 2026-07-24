import { HttpResponse, http } from 'msw';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it } from 'vitest';

import { type Workflow, emptyAction, emptyWorkflow } from '@/workflow/model';
import { renderWithProviders } from '@/test/render';
import { server } from '@/test/server';
import { RunPanel } from './RunPanel';
import { useBuilderStore } from './store';

// RunPanel carries the builder's save/run/test logic; it is unit-mounted on its
// own (BuilderPage also renders the reactflow Canvas, which jsdom lacks a
// ResizeObserver for). Seeding the store gives it a workflow to act on.

function seed(): void {
  const flow: Workflow = {
    ...emptyWorkflow('hello'),
    actions: {
      greet: { ...emptyAction('greet'), runner: 'mock', params: { output: 'hi' } },
    },
  };
  useBuilderStore.getState().loadWorkflow(flow);
}

function ndjson(lines: string[]): Response {
  return new HttpResponse(lines.join('\n') + '\n', {
    headers: { 'Content-Type': 'application/x-ndjson' },
  });
}

describe('builder run panel', () => {
  beforeEach(seed);

  it('maps validation codes onto nodes and does not save when validation fails', async () => {
    let putCalled = false;
    server.use(
      http.post('/v1/workflows/validate', () =>
        HttpResponse.json({
          valid: false,
          errors: [
            {
              code: 'workflow.bad_reference',
              message: 'unknown action',
              details: { action: 'greet' },
            },
          ],
        }),
      ),
      http.put('/v1/workflows/hello', () => {
        putCalled = true;
        return HttpResponse.json({ workflowName: 'hello', hash: 'x' });
      }),
    );

    const user = userEvent.setup();
    renderWithProviders(<RunPanel />, { withAuth: false });

    await user.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      const node = useBuilderStore.getState().nodes.find((item) => item.id === 'greet');
      expect(node?.data.errorCodes).toEqual(['workflow.bad_reference']);
    });
    expect(putCalled).toBe(false);
  });

  it('saves through the PUT route when validation passes', async () => {
    let putBody: { toml?: string } | null = null;
    server.use(
      http.post('/v1/workflows/validate', () => HttpResponse.json({ valid: true, errors: [] })),
      http.put('/v1/workflows/hello', async ({ request }) => {
        putBody = (await request.json()) as { toml?: string };
        return HttpResponse.json({ workflowName: 'hello', hash: 'deadbeef', created: true });
      }),
    );

    const user = userEvent.setup();
    renderWithProviders(<RunPanel />, { withAuth: false });

    await user.click(screen.getByRole('button', { name: 'Save' }));

    await screen.findByText(/deadbeef/);
    expect(putBody).not.toBeNull();
    expect(putBody?.toml).toBeTypeOf('string');
  });

  it('folds run stream events into the live action table', async () => {
    server.use(
      http.post('/v1/workflows/hello/runs', () =>
        ndjson([
          JSON.stringify({ result: { runId: 'r1', type: 'RUN_EVENT_TYPE_RUN_STARTED' } }),
          JSON.stringify({
            result: {
              runId: 'r1',
              type: 'RUN_EVENT_TYPE_ACTION_TRANSITION',
              action: { name: 'greet', status: 'running' },
            },
          }),
          JSON.stringify({
            result: {
              runId: 'r1',
              type: 'RUN_EVENT_TYPE_ACTION_TRANSITION',
              action: { name: 'greet', status: 'succeeded' },
            },
          }),
          JSON.stringify({
            result: {
              runId: 'r1',
              type: 'RUN_EVENT_TYPE_RUN_FINISHED',
              run: { runId: 'r1', workflowName: 'hello', status: 'succeeded' },
            },
          }),
        ]),
      ),
    );

    const user = userEvent.setup();
    renderWithProviders(<RunPanel />, { withAuth: false });

    await user.click(screen.getByRole('button', { name: 'Run' }));

    const rows = await screen.findAllByRole('row');
    expect(rows.slice(1)[0]).toHaveTextContent('greet');
    await screen.findByText('finished', { exact: false });
  });

  it('runs one selected action inline, carrying the definition as toml', async () => {
    let runBody: { inputs: Record<string, string>; toml?: string } | null = null;
    server.use(
      http.post('/v1/workflows/hello/runs', async ({ request }) => {
        runBody = (await request.json()) as { inputs: Record<string, string>; toml?: string };
        return ndjson([
          JSON.stringify({
            result: {
              runId: 'r2',
              type: 'RUN_EVENT_TYPE_RUN_FINISHED',
              run: { runId: 'r2', workflowName: 'hello', status: 'succeeded' },
            },
          }),
        ]);
      }),
    );

    // A selected node enables the test button; test runs it alone.
    useBuilderStore.getState().select('greet');

    const user = userEvent.setup();
    renderWithProviders(<RunPanel />, { withAuth: false });

    await user.click(screen.getByRole('button', { name: 'Test selected action' }));

    await waitFor(() => {
      expect(runBody).not.toBeNull();
    });
    expect(runBody?.toml).toBeTypeOf('string');
  });
});
