import { http } from 'msw';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { errorLine, ndjsonResponse, resultLine, runEventLines } from '@/test/ndjson';
import { server } from '@/test/server';
import { renderWithProviders } from '@/test/render';
import { RunMonitorPage } from './RunMonitorPage';

describe('run monitor', () => {
  it('renders streamed action transitions in order', async () => {
    server.use(
      http.post('/v1/workflows/hello/runs', () =>
        ndjsonResponse(
          runEventLines('r1', [
            ['greet', 'running'],
            ['echo', 'running'],
            ['greet', 'succeeded'],
          ]),
        ),
      ),
    );

    const user = userEvent.setup();
    renderWithProviders(<RunMonitorPage />, { withAuth: false });

    await user.type(screen.getByLabelText('Workflow name'), 'hello');
    await user.click(screen.getByRole('button', { name: 'Run workflow' }));

    // Both actions land in the table; greet appears before echo because that
    // is the order the stream first mentioned them.
    const rows = await screen.findAllByRole('row');
    const dataRows = rows.slice(1); // drop the header row
    expect(dataRows[0]).toHaveTextContent('greet');
    expect(dataRows[1]).toHaveTextContent('echo');

    // greet's final transition wins: succeeded, not the earlier running.
    await waitFor(() => {
      expect(within(dataRows[0]!).getByText('succeeded')).toBeInTheDocument();
    });

    // The run's terminal status shows once the finished event arrives.
    await screen.findByText('finished', { exact: false });
  });

  it('shows the error code when the stream carries a failure', async () => {
    server.use(
      http.post('/v1/workflows/broken/runs', () =>
        ndjsonResponse([
          resultLine({ runId: 'r2', type: 'RUN_EVENT_TYPE_RUN_STARTED' }),
          errorLine('workflow.cycle_detected', 'cycle: a -> b -> a'),
        ]),
      ),
    );

    const user = userEvent.setup();
    renderWithProviders(<RunMonitorPage />, { withAuth: false });

    await user.type(screen.getByLabelText('Workflow name'), 'broken');
    await user.click(screen.getByRole('button', { name: 'Run workflow' }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('workflow.cycle_detected');
  });
});
