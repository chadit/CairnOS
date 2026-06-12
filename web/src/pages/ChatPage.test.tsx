import { HttpResponse, http } from 'msw';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { clearToken, getToken, setToken } from '@/api/token';
import { server } from '@/test/server';
import { renderWithProviders } from '@/test/render';
import { App } from '@/App';
import { ChatPage } from './ChatPage';

function ndjson(lines: string[]): Response {
  const body = lines.join('\n') + '\n';
  return new HttpResponse(body, { headers: { 'Content-Type': 'application/x-ndjson' } });
}

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

describe('chat transcript', () => {
  it('renders assistant chunks and a tool note in stream order', async () => {
    server.use(
      http.post('/v1/chat/sessions/sess-new/messages', () =>
        ndjson([
          JSON.stringify({ result: { type: 'CHAT_EVENT_TYPE_ASSISTANT_CHUNK', text: 'Looking ' } }),
          JSON.stringify({ result: { type: 'CHAT_EVENT_TYPE_TOOL_CALL', tool: 'memory' } }),
          JSON.stringify({ result: { type: 'CHAT_EVENT_TYPE_ASSISTANT_CHUNK', text: 'it up.' } }),
          JSON.stringify({ result: { type: 'CHAT_EVENT_TYPE_TURN_DONE', text: 'Looking it up.' } }),
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
    let confirmCalls = 0;
    server.use(
      http.post('/v1/chat/sessions/sess-new/messages', () =>
        ndjson([
          JSON.stringify({
            result: {
              type: 'CHAT_EVENT_TYPE_CONFIRMATION_REQUIRED',
              proposal: {
                workflowName: 'deploy',
                inputs: { region: 'us-east' },
                estimatedBudgetUsd: 1.5,
              },
            },
          }),
        ]),
      ),
      http.post('/v1/chat/sessions/sess-new/confirm', () => {
        confirmCalls += 1;
        return ndjson([
          JSON.stringify({ result: { type: 'CHAT_EVENT_TYPE_TURN_DONE', text: 'done' } }),
        ]);
      }),
    );

    const user = userEvent.setup();
    renderWithProviders(<ChatPage />, { withAuth: false });
    await openSession(user);

    await user.type(screen.getByLabelText('Message'), 'deploy it');
    await user.click(screen.getByRole('button', { name: 'Send' }));

    // The proposal card shows with the workflow, budget, and inputs.
    await screen.findByText('Confirm before running');
    expect(screen.getByText('deploy')).toBeInTheDocument();
    expect(screen.getByText('$1.50')).toBeInTheDocument();
    expect(screen.getByText(/region/)).toBeInTheDocument();

    // The gate held: no run row appeared and ConfirmAction was never called.
    expect(screen.queryByText(/^run /)).not.toBeInTheDocument();
    expect(confirmCalls).toBe(0);
  });

  it('approving calls confirmAction and renders the resulting run status', async () => {
    let approvePayload: unknown = null;
    server.use(
      http.post('/v1/chat/sessions/sess-new/messages', () =>
        ndjson([
          JSON.stringify({
            result: {
              type: 'CHAT_EVENT_TYPE_CONFIRMATION_REQUIRED',
              proposal: { workflowName: 'deploy', inputs: {}, estimatedBudgetUsd: 0.25 },
            },
          }),
        ]),
      ),
      http.post('/v1/chat/sessions/sess-new/confirm', async ({ request }) => {
        approvePayload = await request.json();
        return ndjson([
          JSON.stringify({ result: { type: 'CHAT_EVENT_TYPE_RUN_STARTED', runId: 'run-7' } }),
          JSON.stringify({
            result: {
              type: 'CHAT_EVENT_TYPE_RUN_FINISHED',
              runId: 'run-7',
              runStatus: 'succeeded',
            },
          }),
        ]);
      }),
    );

    const user = userEvent.setup();
    renderWithProviders(<ChatPage />, { withAuth: false });
    await openSession(user);

    await user.type(screen.getByLabelText('Message'), 'deploy it');
    await user.click(screen.getByRole('button', { name: 'Send' }));

    await screen.findByText('Confirm before running');
    await user.click(screen.getByRole('button', { name: 'Approve and run' }));

    // ConfirmAction fired with approve=true, and the run rows rendered.
    await waitFor(() => expect(approvePayload).toEqual({ approve: true }));
    await screen.findByText('succeeded');
    // The card is gone once the decision is made.
    expect(screen.queryByText('Confirm before running')).not.toBeInTheDocument();
  });

  it('denying calls confirmAction with approve=false and shows no run', async () => {
    let denyPayload: unknown = null;
    server.use(
      http.post('/v1/chat/sessions/sess-new/messages', () =>
        ndjson([
          JSON.stringify({
            result: {
              type: 'CHAT_EVENT_TYPE_CONFIRMATION_REQUIRED',
              proposal: { workflowName: 'deploy', inputs: {}, estimatedBudgetUsd: 0.25 },
            },
          }),
        ]),
      ),
      http.post('/v1/chat/sessions/sess-new/confirm', async ({ request }) => {
        denyPayload = await request.json();
        return ndjson([
          JSON.stringify({
            result: { type: 'CHAT_EVENT_TYPE_ASSISTANT_CHUNK', text: 'Okay, skipping that.' },
          }),
          JSON.stringify({ result: { type: 'CHAT_EVENT_TYPE_TURN_DONE', text: 'Okay.' } }),
        ]);
      }),
    );

    const user = userEvent.setup();
    renderWithProviders(<ChatPage />, { withAuth: false });
    await openSession(user);

    await user.type(screen.getByLabelText('Message'), 'deploy it');
    await user.click(screen.getByRole('button', { name: 'Send' }));

    await screen.findByText('Confirm before running');
    await user.click(screen.getByRole('button', { name: 'Deny' }));

    await waitFor(() => expect(denyPayload).toEqual({ approve: false }));
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
