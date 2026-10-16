import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { server } from '@/test/server';
import { clearToken, getToken, setToken } from './token';
import { type CairnApiError, isCairnApiError, onUnauthorized, request } from './client';
import { listRunners, login, validateWorkflow } from './endpoints';

/** noop is a fresh unauthorized handler so a test's spy does not leak. */
function noop(): void {
  // intentionally empty
}

beforeEach(() => {
  clearToken();
});

afterEach(() => {
  onUnauthorized(noop);
});

describe('error envelope unwrap', () => {
  it('turns the canonical envelope into a CairnApiError with the code', async () => {
    server.use(
      http.get('/v1/runners', () =>
        HttpResponse.json(
          {
            status: 'error',
            error: {
              code: 'auth.forbidden',
              message: 'admin scope required',
              details: { scope: 'admin' },
            },
          },
          { status: 403 },
        ),
      ),
    );

    const caught = await listRunners().catch((err: unknown) => err);
    expect(isCairnApiError(caught)).toBe(true);
    expect((caught as CairnApiError).code).toBe('auth.forbidden');
    expect((caught as CairnApiError).message).toBe('admin scope required');
    expect((caught as CairnApiError).details).toEqual({ scope: 'admin' });
  });

  it('falls back to a generic code when the body is a bare grpc status', async () => {
    server.use(
      http.get('/v1/runners', () =>
        HttpResponse.json({ code: 13, message: 'internal' }, { status: 500 }),
      ),
    );

    const caught = await listRunners().catch((err: unknown) => err);
    expect((caught as CairnApiError).code).toBe('gateway.error');
    expect((caught as CairnApiError).message).toBe('internal');
  });

  it('fires the unauthorized handler and clears the token on a 401', async () => {
    setToken('stale-token');
    const onUnauth = vi.fn<() => void>();
    onUnauthorized(onUnauth);

    server.use(
      http.get('/v1/runners', () =>
        HttpResponse.json(
          { status: 'error', error: { code: 'auth.unauthorized', message: 'token expired' } },
          { status: 401 },
        ),
      ),
    );

    await listRunners().catch(() => undefined);

    expect(onUnauth).toHaveBeenCalledOnce();
    expect(getToken()).toBeNull();
  });
});

describe('auth header', () => {
  it('attaches the bearer token once stored', async () => {
    setToken('abc123');
    let seen: string | null = null;
    server.use(
      http.get('/v1/runners', ({ request: req }) => {
        seen = req.headers.get('Authorization');
        return HttpResponse.json({ runners: [] });
      }),
    );

    await listRunners();
    expect(seen).toBe('Bearer abc123');
  });

  it('login stores the returned token for later calls', async () => {
    await login('valid-key');
    expect(getToken()).toBe('test-token');
  });
});

describe('validate request encoding', () => {
  it('base64-encodes the TOML for the bytes field', async () => {
    let sentToml: string | null = null;
    server.use(
      http.post('/v1/workflows/validate', async ({ request: req }) => {
        const body = (await req.json()) as { toml?: string };
        sentToml = body.toml ?? null;
        return HttpResponse.json({ valid: true, errors: [] });
      }),
    );

    await validateWorkflow('[workflow]\nname = "w"\n');
    expect(sentToml).not.toBeNull();
    // Decoding the bytes field must reproduce the original TOML.
    expect(atob(sentToml!)).toBe('[workflow]\nname = "w"\n');
  });
});

describe('request', () => {
  it('builds a query string from array and scalar params', async () => {
    let url: string | null = null;
    server.use(
      http.get('/v1/memory', ({ request: req }) => {
        url = req.url;
        return HttpResponse.json({ entries: [] });
      }),
    );

    await request('/memory', { query: { text: 'hi', tags: ['a', 'b'], limit: 5 } });
    const parsed = new URL(url!);
    expect(parsed.searchParams.get('text')).toBe('hi');
    expect(parsed.searchParams.getAll('tags')).toEqual(['a', 'b']);
    expect(parsed.searchParams.get('limit')).toBe('5');
  });
});
