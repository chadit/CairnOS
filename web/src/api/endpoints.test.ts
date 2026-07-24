import { HttpResponse, http } from 'msw';
import { describe, expect, it } from 'vitest';

import { server } from '@/test/server';
import { isCairnApiError } from './client';
import { base64Encode, saveWorkflow } from './endpoints';

// saveWorkflow rides the same base64 bytes mapping validateWorkflow uses. These
// check the PUT carries the encoded TOML and that a gateway error envelope
// surfaces as the typed CairnApiError, not a raw fetch rejection.

describe('saveWorkflow', () => {
  it('PUTs base64-encoded TOML to the named route and returns the save response', async () => {
    let captured: { toml?: string } | null = null;
    let method: string | null = null;
    server.use(
      http.put('/v1/workflows/hello', async ({ request }) => {
        method = request.method;
        captured = (await request.json()) as { toml?: string };
        return HttpResponse.json({ workflowName: 'hello', hash: 'abc123', created: true });
      }),
    );

    const res = await saveWorkflow('hello', 'name = "hello"\n');

    expect(res).toEqual({ workflowName: 'hello', hash: 'abc123', created: true });
    expect(method).toBe('PUT');
    expect(captured).toEqual({ toml: base64Encode('name = "hello"\n') });
  });

  it('surfaces the gateway error envelope as a CairnApiError', async () => {
    server.use(
      http.put('/v1/workflows/bad', () =>
        HttpResponse.json(
          {
            status: 'error',
            error: { code: 'api.workflow_name_mismatch', message: 'name mismatch' },
          },
          { status: 400 },
        ),
      ),
    );

    let caught: unknown = null;
    try {
      await saveWorkflow('bad', 'name = "other"\n');
    } catch (err) {
      caught = err;
    }

    expect(isCairnApiError(caught)).toBe(true);
    if (isCairnApiError(caught)) {
      expect(caught.code).toBe('api.workflow_name_mismatch');
    }
  });
});
