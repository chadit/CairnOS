import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithProviders } from '@/test/render';
import { LiveRunPanel } from './LiveRunPanel';
import type { RunStreamState } from './useRunStream';

function runningState(overrides: Partial<RunStreamState> = {}): RunStreamState {
  return {
    running: true,
    runId: 'r1',
    finalStatus: null,
    actions: [{ name: 'greet', status: 'running', attempt: 0 }],
    events: [],
    error: null,
    ...overrides,
  };
}

describe('LiveRunPanel', () => {
  it('renders the per-action live table', () => {
    renderWithProviders(<LiveRunPanel state={runningState()} cancel={vi.fn<() => void>()} />, {
      withAuth: false,
    });

    const rows = screen.getAllByRole('row');
    expect(rows.slice(1)[0]).toHaveTextContent('greet');
  });

  it('shows the terminal status once the run finishes', () => {
    renderWithProviders(
      <LiveRunPanel
        state={runningState({ running: false, finalStatus: 'succeeded' })}
        cancel={vi.fn<() => void>()}
      />,
      { withAuth: false },
    );

    expect(screen.getByText('finished', { exact: false })).toBeInTheDocument();
  });

  it('invokes cancel from its button while the run is live', async () => {
    const cancel = vi.fn<() => void>();
    const user = userEvent.setup();
    renderWithProviders(<LiveRunPanel state={runningState()} cancel={cancel} />, {
      withAuth: false,
    });

    await user.click(screen.getByRole('button', { name: 'Cancel' }));

    expect(cancel).toHaveBeenCalledOnce();
  });
});
