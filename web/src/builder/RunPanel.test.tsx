import { HttpResponse, http } from 'msw';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it } from 'vitest';

import { type Workflow, emptyAction, emptyWorkflow } from '@/workflow/model';
import { ndjsonResponse, runEventLines, runFinishedLine } from '@/test/ndjson';
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

/** mockSaveRoute answers the workflow PUT with `hash` and records the body it
 * received. A null body means the route was never reached, which is what the
 * validation-failure case asserts. */
function mockSaveRoute(hash: string): () => { toml?: string } | null {
  let body: { toml?: string } | null = null;
  server.use(
    http.put('/v1/workflows/hello', async ({ request }) => {
      body = (await request.json()) as { toml?: string };
      return HttpResponse.json({ workflowName: 'hello', hash, created: true });
    }),
  );
  return () => body;
}

/** clickSave mounts the panel and presses Save, the entry point both
 * validate-then-save cases share. */
async function clickSave(): Promise<void> {
  const user = userEvent.setup();
  renderWithProviders(<RunPanel />, { withAuth: false });
  await user.click(screen.getByRole('button', { name: 'Save' }));
}

describe('builder run panel', () => {
  beforeEach(seed);

  it('maps validation codes onto nodes and does not save when validation fails', async () => {
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
    );
    const savedBody = mockSaveRoute('x');

    await clickSave();

    await waitFor(() => {
      const node = useBuilderStore.getState().nodes.find((item) => item.id === 'greet');
      expect(node?.data.errorCodes).toEqual(['workflow.bad_reference']);
    });
    expect(savedBody()).toBeNull();
  });

  it('saves through the PUT route when validation passes', async () => {
    server.use(
      http.post('/v1/workflows/validate', () => HttpResponse.json({ valid: true, errors: [] })),
    );
    const savedBody = mockSaveRoute('deadbeef');

    await clickSave();

    await screen.findByText(/deadbeef/);
    expect(savedBody()).not.toBeNull();
    expect(savedBody()?.toml).toBeTypeOf('string');
  });

  it('folds run stream events into the live action table', async () => {
    server.use(
      http.post('/v1/workflows/hello/runs', () =>
        ndjsonResponse(
          runEventLines('r1', [
            ['greet', 'running'],
            ['greet', 'succeeded'],
          ]),
        ),
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
    // Only the handler assigns runBody, which flow analysis cannot see, so the
    // assertion keeps the declared type from narrowing to null.
    let runBody = null as { inputs: Record<string, string>; toml?: string } | null;
    server.use(
      http.post('/v1/workflows/hello/runs', async ({ request }) => {
        runBody = (await request.json()) as { inputs: Record<string, string>; toml?: string };
        return ndjsonResponse([runFinishedLine('r2')]);
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
