import { HttpResponse, http } from 'msw';
import { describe, expect, it } from 'vitest';

import {
  chunkedNdjsonResponse,
  errorLine,
  resultLine,
  runEventLines,
  runFinishedLine,
} from '@/test/ndjson';
import { server } from '@/test/server';
import { isCairnApiError } from './client';
import { base64Encode } from './endpoints';
import { streamRunEvents } from './stream';

// The gateway delivers RunWorkflow as newline-delimited JSON, one
// {result|error} envelope per line. These tests feed a mocked NDJSON body and
// check the consumer yields RunEvents in stream order and surfaces a
// mid-stream error envelope as a CairnApiError.

/** eventTypes drains a run stream that ends cleanly and returns each event's
 * type in arrival order. */
async function eventTypes(...args: Parameters<typeof streamRunEvents>): Promise<string[]> {
  const types: string[] = [];
  for await (const event of streamRunEvents(...args)) {
    types.push(event.type);
  }
  return types;
}

describe('streamRunEvents', () => {
  it('yields events in the order the stream sent them', async () => {
    server.use(
      http.post('/v1/workflows/hello/runs', () =>
        chunkedNdjsonResponse(
          runEventLines('r1', [
            ['greet', 'running'],
            ['greet', 'succeeded'],
          ]),
        ),
      ),
    );

    expect(await eventTypes('hello', {})).toEqual([
      'RUN_EVENT_TYPE_RUN_STARTED',
      'RUN_EVENT_TYPE_ACTION_TRANSITION',
      'RUN_EVENT_TYPE_ACTION_TRANSITION',
      'RUN_EVENT_TYPE_RUN_FINISHED',
    ]);
  });

  it('skips blank lines and keeps a final line that has no trailing newline', async () => {
    server.use(
      http.post(
        '/v1/workflows/hello/runs',
        () =>
          new HttpResponse(
            [
              resultLine({ runId: 'r1', type: 'RUN_EVENT_TYPE_RUN_STARTED' }),
              '',
              runFinishedLine('r1'),
            ].join('\n'),
            { headers: { 'Content-Type': 'application/x-ndjson' } },
          ),
      ),
    );

    expect(await eventTypes('hello', {})).toEqual([
      'RUN_EVENT_TYPE_RUN_STARTED',
      'RUN_EVENT_TYPE_RUN_FINISHED',
    ]);
  });

  it('throws a CairnApiError when a stream line carries an error envelope', async () => {
    server.use(
      http.post('/v1/workflows/hello/runs', () =>
        chunkedNdjsonResponse([
          resultLine({ runId: 'r1', type: 'RUN_EVENT_TYPE_RUN_STARTED' }),
          errorLine('workflow.cycle_detected', 'cycle: a -> b -> a'),
        ]),
      ),
    );

    const seen: string[] = [];
    let caught: unknown = null;
    try {
      for await (const event of streamRunEvents('hello', {})) {
        seen.push(event.type);
      }
    } catch (err) {
      caught = err;
    }

    expect(seen).toEqual(['RUN_EVENT_TYPE_RUN_STARTED']);
    expect(isCairnApiError(caught)).toBe(true);
    expect(caught).toHaveProperty('code', 'workflow.cycle_detected');
  });

  it('sends the inline definition as base64 toml only when the option is set', async () => {
    const bodies: { inputs: Record<string, string>; toml?: string }[] = [];
    server.use(
      http.post('/v1/workflows/hello/runs', async ({ request }) => {
        bodies.push((await request.json()) as { inputs: Record<string, string>; toml?: string });
        return chunkedNdjsonResponse([runFinishedLine('r')]);
      }),
    );

    for await (const _event of streamRunEvents('hello', { a: '1' })) {
      void _event;
    }
    for await (const _event of streamRunEvents('hello', { a: '1' }, { toml: 'name = "x"\n' })) {
      void _event;
    }

    // No option: the body is just the inputs, no toml key at all.
    expect(bodies[0]).toEqual({ inputs: { a: '1' } });
    // With the option: the same inputs plus the base64-encoded definition.
    expect(bodies[1]).toEqual({ inputs: { a: '1' }, toml: base64Encode('name = "x"\n') });
  });

  it('surfaces a non-2xx start as a CairnApiError with the envelope code', async () => {
    server.use(
      http.post('/v1/workflows/missing/runs', () =>
        HttpResponse.json(
          { status: 'error', error: { code: 'workflow.unknown', message: 'no such workflow' } },
          { status: 404 },
        ),
      ),
    );

    let caught: unknown = null;
    const seen: string[] = [];
    try {
      for await (const event of streamRunEvents('missing', {})) {
        seen.push(event.type);
      }
    } catch (err) {
      caught = err;
    }

    expect(seen).toEqual([]);

    expect(isCairnApiError(caught)).toBe(true);
    expect(caught).toHaveProperty('message', 'no such workflow');
  });
});
