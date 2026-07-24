import { HttpResponse, http } from 'msw';

// Default handlers model the gateway's documented happy path so a component
// renders without each test re-stating the whole API. Tests override any of
// these with server.use(...) to exercise errors, streams, and edge cases.
// Shapes here come from internal/api/gen/openapi/cairnos.swagger.json, not
// from the client, so a client bug cannot make a test pass by accident.

export const handlers = [
  http.get('/v1/health', () => HttpResponse.json({ status: 'ok' })),

  http.post('/v1/login', async ({ request }) => {
    const body = (await request.json()) as { key?: string };
    if (!body.key) {
      return HttpResponse.json(
        { status: 'error', error: { code: 'auth.invalid_key', message: 'key is required' } },
        { status: 400 },
      );
    }
    return HttpResponse.json({ token: 'test-token', expiresAt: '2030-01-01T00:00:00Z' });
  }),

  http.get('/v1/runners', () =>
    HttpResponse.json({ runners: [{ name: 'mock' }, { name: 'email' }, { name: 'webhook' }] }),
  ),

  http.get('/v1/runs', () => HttpResponse.json({ runs: [] })),

  http.get('/v1/suggestions', () => HttpResponse.json({ suggestions: [] })),

  http.post('/v1/workflows/validate', () => HttpResponse.json({ valid: true, errors: [] })),

  http.put('/v1/workflows/:name', ({ params }) =>
    HttpResponse.json({ workflowName: String(params.name), hash: 'test-hash', created: true }),
  ),

  http.get('/v1/chat/sessions', () => HttpResponse.json({ sessions: [] })),

  http.post('/v1/chat/sessions', async ({ request }) => {
    const body = (await request.json()) as { personality?: string };
    return HttpResponse.json({
      session: {
        id: 'sess-new',
        personality: body.personality ?? '',
        createdAt: '2030-01-01T00:00:00Z',
        updatedAt: '2030-01-01T00:00:00Z',
      },
    });
  }),

  http.get('/v1/chat/sessions/:sessionId/messages', () => HttpResponse.json({ messages: [] })),
];
