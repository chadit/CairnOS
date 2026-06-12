import type { ReactElement, ReactNode } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter } from 'react-router-dom';
import { type RenderOptions, render } from '@testing-library/react';

import { AuthProvider } from '@/auth/AuthContext';

/** newTestClient builds a QueryClient with retries off so a mocked error
 * surfaces on the first attempt instead of looping. */
function newTestClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
}

interface ProviderOptions extends Omit<RenderOptions, 'wrapper'> {
  /** route seeds the MemoryRouter's initial entry. */
  route?: string;
  /** withAuth wraps the tree in the AuthProvider; off for unit-only renders. */
  withAuth?: boolean;
}

/**
 * renderWithProviders mounts a component inside the query, router, and auth
 * providers the app uses, so a test exercises the same wiring as production.
 */
export function renderWithProviders(ui: ReactElement, options: ProviderOptions = {}) {
  const { route = '/', withAuth = true, ...rest } = options;
  const client = newTestClient();

  function Wrapper({ children }: { children: ReactNode }) {
    const routed = <MemoryRouter initialEntries={[route]}>{children}</MemoryRouter>;
    const authed = withAuth ? <AuthProvider>{routed}</AuthProvider> : routed;
    return <QueryClientProvider client={client}>{authed}</QueryClientProvider>;
  }

  return { client, ...render(ui, { wrapper: Wrapper, ...rest }) };
}
