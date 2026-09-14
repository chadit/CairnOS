import { http } from 'msw';
import { describe, expect, it } from 'vitest';

import { chunkedNdjsonResponse, errorLine, resultLine } from '@/test/ndjson';
import { server } from '@/test/server';
import { confirmAction, sendMessage } from './chat';
import { isCairnApiError } from './client';

// SendMessage and ConfirmAction stream ChatEvents as NDJSON, one
// {result|error} envelope per line. These tests feed a mocked NDJSON body and
// check the consumer yields events in stream order, including a tool_call
// between assistant chunks, and surfaces a mid-stream error envelope as a
// CairnApiError.

describe('sendMessage stream', () => {
  it('yields assistant_chunk and tool_call events in send order', async () => {
    server.use(
      http.post('/v1/chat/sessions/s1/messages', () =>
        chunkedNdjsonResponse([
          resultLine({ type: 'CHAT_EVENT_TYPE_ASSISTANT_CHUNK', text: 'Let me ' }),
          resultLine({ type: 'CHAT_EVENT_TYPE_TOOL_CALL', tool: 'memory' }),
          resultLine({ type: 'CHAT_EVENT_TYPE_ASSISTANT_CHUNK', text: 'check.' }),
          resultLine({ type: 'CHAT_EVENT_TYPE_TURN_DONE', text: 'Let me check.' }),
        ]),
      ),
    );

    const types: string[] = [];
    const texts: string[] = [];
    for await (const event of sendMessage('s1', 'hi')) {
      types.push(event.type);
      if (event.text) {
        texts.push(event.text);
      }
    }

    expect(types).toEqual([
      'CHAT_EVENT_TYPE_ASSISTANT_CHUNK',
      'CHAT_EVENT_TYPE_TOOL_CALL',
      'CHAT_EVENT_TYPE_ASSISTANT_CHUNK',
      'CHAT_EVENT_TYPE_TURN_DONE',
    ]);
    expect(texts).toEqual(['Let me ', 'check.', 'Let me check.']);
  });

  it('throws a CairnApiError when a stream line carries an error envelope', async () => {
    server.use(
      http.post('/v1/chat/sessions/s1/messages', () =>
        chunkedNdjsonResponse([
          resultLine({ type: 'CHAT_EVENT_TYPE_ASSISTANT_CHUNK', text: 'hmm' }),
          errorLine('chat.model_unavailable', 'model is down'),
        ]),
      ),
    );

    const seen: string[] = [];
    let caught: unknown = null;
    try {
      for await (const event of sendMessage('s1', 'hi')) {
        seen.push(event.type);
      }
    } catch (err) {
      caught = err;
    }

    expect(seen).toEqual(['CHAT_EVENT_TYPE_ASSISTANT_CHUNK']);
    expect(isCairnApiError(caught)).toBe(true);
    if (isCairnApiError(caught)) {
      expect(caught.code).toBe('chat.model_unavailable');
    }
  });
});

describe('confirmAction stream', () => {
  it('posts the approve decision and yields the follow-on run events', async () => {
    let posted: unknown = null;
    server.use(
      http.post('/v1/chat/sessions/s1/confirm', async ({ request }) => {
        posted = await request.json();
        return chunkedNdjsonResponse([
          resultLine({ type: 'CHAT_EVENT_TYPE_RUN_STARTED', runId: 'r9' }),
          resultLine({
            type: 'CHAT_EVENT_TYPE_RUN_FINISHED',
            runId: 'r9',
            runStatus: 'succeeded',
          }),
        ]);
      }),
    );

    const types: string[] = [];
    for await (const event of confirmAction('s1', true)) {
      types.push(event.type);
    }

    expect(posted).toEqual({ approve: true });
    expect(types).toEqual(['CHAT_EVENT_TYPE_RUN_STARTED', 'CHAT_EVENT_TYPE_RUN_FINISHED']);
  });
});
