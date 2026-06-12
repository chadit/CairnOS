import { HttpResponse, http } from 'msw';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { clearToken, getToken } from '@/api/token';
import { server } from '@/test/server';
import { renderWithProviders } from '@/test/render';
import { App } from '@/App';
import { LoginPage } from './LoginPage';

beforeEach(() => {
  clearToken();
});

afterEach(() => {
  clearToken();
});

describe('login flow', () => {
  it('stores the token and attaches it on the next call', async () => {
    let runnersAuth: string | null = null;
    server.use(
      http.get('/v1/runners', ({ request }) => {
        runnersAuth = request.headers.get('Authorization');
        return HttpResponse.json({ runners: [{ name: 'mock' }] });
      }),
    );

    const user = userEvent.setup();
    renderWithProviders(<App />, { route: '/runners' });

    await user.type(screen.getByLabelText('Access key'), 'good-key');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));

    // After sign-in the shell renders and the runners call carries the token.
    await waitFor(() => expect(getToken()).toBe('test-token'));
    await screen.findByRole('heading', { name: 'Runners' });
    expect(runnersAuth).toBe('Bearer test-token');
  });

  it('shows the canonical error code when sign-in fails', async () => {
    server.use(
      http.post('/v1/login', () =>
        HttpResponse.json(
          { status: 'error', error: { code: 'auth.invalid_key', message: 'unknown key' } },
          { status: 401 },
        ),
      ),
    );

    const user = userEvent.setup();
    renderWithProviders(<LoginPage />);

    await user.type(screen.getByLabelText('Access key'), 'bad-key');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('auth.invalid_key');
    expect(alert).toHaveTextContent('unknown key');
  });

  it('routes back to login when a guarded call returns 401', async () => {
    // Start signed in, then have the first guarded call reject with 401. The
    // app must drop back to the login screen.
    server.use(
      http.get('/v1/suggestions', () =>
        HttpResponse.json(
          { status: 'error', error: { code: 'auth.unauthorized', message: 'token expired' } },
          { status: 401 },
        ),
      ),
    );

    const user = userEvent.setup();
    renderWithProviders(<App />, { route: '/suggestions' });

    // Sign in first so the shell renders.
    await user.type(screen.getByLabelText('Access key'), 'good-key');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));

    // The suggestions call 401s, the auth context flips, login returns.
    await screen.findByRole('button', { name: 'Sign in' });
    expect(getToken()).toBeNull();
  });
});
