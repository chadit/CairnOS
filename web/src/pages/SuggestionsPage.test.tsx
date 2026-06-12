import { HttpResponse, http } from 'msw';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { server } from '@/test/server';
import { renderWithProviders } from '@/test/render';
import { SuggestionsPage } from './SuggestionsPage';

const openSuggestion = {
  id: 's1',
  kind: 'caching',
  target: 'career_scout.score_jobs',
  status: 'open',
  rationale: 'identical inputs hit the model 40 times an hour',
  proposed: { cache: { ttl: '1h' } },
};

describe('suggestions', () => {
  it('approve calls the approve endpoint and shows the returned annotation', async () => {
    let approveCalledFor: string | null = null;
    server.use(
      http.get('/v1/suggestions', () => HttpResponse.json({ suggestions: [openSuggestion] })),
      http.post('/v1/suggestions/:id/approve', ({ params }) => {
        approveCalledFor = String(params.id);
        return HttpResponse.json({
          suggestion: { ...openSuggestion, status: 'approved' },
          annotation: '# add to career_scout.toml\n[action.score_jobs.cache]\nttl = "1h"',
        });
      }),
    );

    const user = userEvent.setup();
    renderWithProviders(<SuggestionsPage />, { withAuth: false });

    await user.click(await screen.findByRole('button', { name: 'Approve' }));

    await waitFor(() => expect(approveCalledFor).toBe('s1'));
    // The human-apply annotation is shown read-only for the operator to apply.
    expect(await screen.findByText(/add to career_scout.toml/)).toBeInTheDocument();
  });

  it('reject calls the reject endpoint', async () => {
    let rejectCalled = false;
    server.use(
      http.get('/v1/suggestions', () => HttpResponse.json({ suggestions: [openSuggestion] })),
      http.post('/v1/suggestions/:id/reject', () => {
        rejectCalled = true;
        return HttpResponse.json({ suggestion: { ...openSuggestion, status: 'rejected' } });
      }),
    );

    const user = userEvent.setup();
    renderWithProviders(<SuggestionsPage />, { withAuth: false });

    await user.click(await screen.findByRole('button', { name: 'Reject' }));
    await waitFor(() => expect(rejectCalled).toBe(true));
  });

  it('surfaces the error envelope when approve fails', async () => {
    server.use(
      http.get('/v1/suggestions', () => HttpResponse.json({ suggestions: [openSuggestion] })),
      http.post('/v1/suggestions/:id/approve', () =>
        HttpResponse.json(
          { status: 'error', error: { code: 'auth.forbidden', message: 'admin scope required' } },
          { status: 403 },
        ),
      ),
    );

    const user = userEvent.setup();
    renderWithProviders(<SuggestionsPage />, { withAuth: false });

    await user.click(await screen.findByRole('button', { name: 'Approve' }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('auth.forbidden');
  });
});
