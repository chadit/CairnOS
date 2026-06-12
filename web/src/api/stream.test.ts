import { HttpResponse, http } from 'msw';
import { describe, expect, it } from 'vitest';

import { server } from '@/test/server';
import { isCairnApiError } from './client';
import { streamRunEvents } from './stream';

// The gateway delivers RunWorkflow as newline-delimited JSON, one
// {result|error} envelope per line. These tests feed a mocked NDJSON body and
// check the consumer yields RunEvents in stream order and surfaces a
// mid-stream error envelope as a CairnApiError.

function ndjsonResponse(lines: string[]): Response {
  // Build a real chunked stream so the reader path (not a single JSON parse)
  // is exercised, with a line split across chunk boundaries.
  const encoder = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      const joined = lines.join('\n') + '\n';
      const mid = Math.floor(joined.length / 2);
      controller.enqueue(encoder.encode(joined.slice(0, mid)));
      controller.enqueue(encoder.encode(joined.slice(mid)));
      controller.close();
    },
  });
  return new HttpResponse(body, { headers: { 'Content-Type': 'application/x-ndjson' } });
}

describe('streamRunEvents', () => {
  it('yields events in the order the stream sent them', async () => {
    server.use(
      http.post('/v1/workflows/hello/runs', () =>
        ndjsonResponse([
          JSON.stringify({ result: { runId: 'r1', type: 'RUN_EVENT_TYPE_RUN_STARTED' } }),
          JSON.stringify({
            result: {
              runId: 'r1',
              type: 'RUN_EVENT_TYPE_ACTION_TRANSITION',
              action: { name: 'greet', status: 'running' },
            },
          }),
          JSON.stringify({
            result: {
              runId: 'r1',
              type: 'RUN_EVENT_TYPE_ACTION_TRANSITION',
              action: { name: 'greet', status: 'succeeded' },
            },
          }),
          JSON.stringify({
            result: {
              runId: 'r1',
              type: 'RUN_EVENT_TYPE_RUN_FINISHED',
              run: { runId: 'r1', workflowName: 'hello', status: 'succeeded' },
            },
          }),
        ]),
      ),
    );

    const types: string[] = [];
    for await (const event of streamRunEvents('hello', {})) {
      types.push(event.type);
    }

    expect(types).toEqual([
      'RUN_EVENT_TYPE_RUN_STARTED',
      'RUN_EVENT_TYPE_ACTION_TRANSITION',
      'RUN_EVENT_TYPE_ACTION_TRANSITION',
      'RUN_EVENT_TYPE_RUN_FINISHED',
    ]);
  });

  it('throws a CairnApiError when a stream line carries an error envelope', async () => {
    server.use(
      http.post('/v1/workflows/hello/runs', () =>
        ndjsonResponse([
          JSON.stringify({ result: { runId: 'r1', type: 'RUN_EVENT_TYPE_RUN_STARTED' } }),
          JSON.stringify({
            error: { code: 'workflow.cycle_detected', message: 'cycle: a -> b -> a' },
          }),
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
    if (isCairnApiError(caught)) {
      expect(caught.code).toBe('workflow.cycle_detected');
    }
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
    if (isCairnApiError(caught)) {
      expect(caught.message).toBe('no such workflow');
    }
  });
});
