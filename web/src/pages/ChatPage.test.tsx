import { HttpResponse, http } from 'msw';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { clearToken, getToken, setToken } from '@/api/token';
import { ndjsonResponse, resultLine } from '@/test/ndjson';
import { server } from '@/test/server';
import { renderWithProviders } from '@/test/render';
import { App } from '@/App';
import { ChatPage } from './ChatPage';

beforeEach(() => {
  clearToken();
});

afterEach(() => {
  clearToken();
});

/** openSession starts a new session and waits for the composer to appear, the
 * shared setup for the transcript-level tests below. */
async function openSession(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('button', { name: 'New session' }));
  await screen.findByLabelText('Message');
}

/**
 * mockConfirmFlow answers SendMessage with a single confirmation_required
 * proposal and ConfirmAction with `confirmReply`. The returned readers let a
 * case assert what the confirm route received, and that it was never called at
 * all while the gate holds.
 */
function mockConfirmFlow(
  proposal: Record<string, unknown>,
  confirmReply: string[],
): { calls: () => number; body: () => unknown } {
  let calls = 0;
  let body: unknown = null;
  server.use(
    http.post('/v1/chat/sessions/sess-new/messages', () =>
      ndjsonResponse([resultLine({ type: 'CHAT_EVENT_TYPE_CONFIRMATION_REQUIRED', proposal })]),
    ),
    http.post('/v1/chat/sessions/sess-new/confirm', async ({ request }) => {
      calls += 1;
      body = await request.json();
      return ndjsonResponse(confirmReply);
    }),
  );
  return { calls: () => calls, body: () => body };
}

/** reachConfirmationCard opens a session, sends a message, and waits for the
 * proposal card. It returns the user-event session so the caller can click
 * through the decision buttons. */
async function reachConfirmationCard() {
  const user = userEvent.setup();
  renderWithProviders(<ChatPage />, { withAuth: false });
  await openSession(user);

  await user.type(screen.getByLabelText('Message'), 'deploy it');
  await user.click(screen.getByRole('button', { name: 'Send' }));

  await screen.findByText('Confirm before running');
  return user;
}

describe('chat transcript', () => {
  it('renders assistant chunks and a tool note in stream order', async () => {
    server.use(
      http.post('/v1/chat/sessions/sess-new/messages', () =>
        ndjsonResponse([
          resultLine({ type: 'CHAT_EVENT_TYPE_ASSISTANT_CHUNK', text: 'Looking ' }),
          resultLine({ type: 'CHAT_EVENT_TYPE_TOOL_CALL', tool: 'memory' }),
          resultLine({ type: 'CHAT_EVENT_TYPE_ASSISTANT_CHUNK', text: 'it up.' }),
          resultLine({ type: 'CHAT_EVENT_TYPE_TURN_DONE', text: 'Looking it up.' }),
        ]),
      ),
    );

    const user = userEvent.setup();
    renderWithProviders(<ChatPage />, { withAuth: false });
    await openSession(user);

    await user.type(screen.getByLabelText('Message'), 'find my note');
    await user.click(screen.getByRole('button', { name: 'Send' }));

    // Chunks coalesce into one assistant bubble; the tool note shows once.
    await screen.findByText('Looking it up.');
    expect(screen.getByText('memory')).toBeInTheDocument();
    expect(screen.getByText('find my note')).toBeInTheDocument();
  });

  it('parks a proposal on confirmation_required and does not start a run', async () => {
    const confirm = mockConfirmFlow(
      { workflowName: 'deploy', inputs: { region: 'us-east' }, estimatedBudgetUsd: 1.5 },
      [resultLine({ type: 'CHAT_EVENT_TYPE_TURN_DONE', text: 'done' })],
    );

    await reachConfirmationCard();

    // The proposal card shows with the workflow, budget, and inputs.
    expect(screen.getByText('deploy')).toBeInTheDocument();
    expect(screen.getByText('$1.50')).toBeInTheDocument();
    expect(screen.getByText(/region/)).toBeInTheDocument();

    // The gate held: no run row appeared and ConfirmAction was never called.
    expect(screen.queryByText(/^run /)).not.toBeInTheDocument();
    expect(confirm.calls()).toBe(0);
  });

  it('approving calls confirmAction and renders the resulting run status', async () => {
    const confirm = mockConfirmFlow(
      { workflowName: 'deploy', inputs: {}, estimatedBudgetUsd: 0.25 },
      [
        resultLine({ type: 'CHAT_EVENT_TYPE_RUN_STARTED', runId: 'run-7' }),
        resultLine({
          type: 'CHAT_EVENT_TYPE_RUN_FINISHED',
          runId: 'run-7',
          runStatus: 'succeeded',
        }),
      ],
    );

    const user = await reachConfirmationCard();
    await user.click(screen.getByRole('button', { name: 'Approve and run' }));

    // ConfirmAction fired with approve=true, and the run rows rendered.
    await waitFor(() => expect(confirm.body()).toEqual({ approve: true }));
    await screen.findByText('succeeded');
    // The card is gone once the decision is made.
    expect(screen.queryByText('Confirm before running')).not.toBeInTheDocument();
  });

  it('denying calls confirmAction with approve=false and shows no run', async () => {
    const confirm = mockConfirmFlow(
      { workflowName: 'deploy', inputs: {}, estimatedBudgetUsd: 0.25 },
      [
        resultLine({ type: 'CHAT_EVENT_TYPE_ASSISTANT_CHUNK', text: 'Okay, skipping that.' }),
        resultLine({ type: 'CHAT_EVENT_TYPE_TURN_DONE', text: 'Okay.' }),
      ],
    );

    const user = await reachConfirmationCard();
    await user.click(screen.getByRole('button', { name: 'Deny' }));

    await waitFor(() => expect(confirm.body()).toEqual({ approve: false }));
    await screen.findByText('Okay, skipping that.');
    // Deny means no workflow ran: no run status row.
    expect(screen.queryByText(/^run /)).not.toBeInTheDocument();
  });
});

describe('chat sessions', () => {
  it('lists existing sessions and starts a new one with a personality', async () => {
    let createBody: unknown = null;
    server.use(
      http.get('/v1/chat/sessions', () =>
        HttpResponse.json({
          sessions: [{ id: 'sess-a', personality: 'thorough' }],
        }),
      ),
      http.post('/v1/chat/sessions', async ({ request }) => {
        createBody = await request.json();
        return HttpResponse.json({ session: { id: 'sess-new', personality: 'playful' } });
      }),
    );

    const user = userEvent.setup();
    renderWithProviders(<ChatPage />, { withAuth: false });

    // The existing session is listed.
    await screen.findByText('sess-a');

    // Pick a personality, then start a session; the request carries it.
    await user.selectOptions(screen.getByLabelText('Personality'), 'playful');
    await user.click(screen.getByRole('button', { name: 'New session' }));

    await waitFor(() => expect(createBody).toEqual({ personality: 'playful' }));
    // The new session becomes active and its composer shows.
    await screen.findByLabelText('Message');
  });
});

describe('chat auth gate', () => {
  it('routes back to login when a guarded chat call returns 401', async () => {
    setToken('test-token');
    server.use(
      http.get('/v1/chat/sessions', () =>
        HttpResponse.json(
          { status: 'error', error: { code: 'auth.unauthorized', message: 'token expired' } },
          { status: 401 },
        ),
      ),
    );

    renderWithProviders(<App />, { route: '/chat' });

    // The 401 drops the session and the login screen returns.
    await screen.findByRole('button', { name: 'Sign in' });
    expect(getToken()).toBeNull();
  });
});
