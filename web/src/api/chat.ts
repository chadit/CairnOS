import type {
  ChatEvent,
  CreateSessionResponse,
  ListMessagesResponse,
  ListSessionsResponse,
  StreamEnvelope,
} from './types';
import { CairnApiError, request } from './client';
import { clearToken, getToken } from './token';

// ChatService client. The unary calls reuse the shared `request` (auth attach,
// error-envelope unwrap, 401 handling). The two streaming calls (SendMessage,
// ConfirmAction) hold a long-lived response and parse NDJSON by hand, mirroring
// stream.ts: each line is a {result|error} envelope and an `error` line aborts
// the stream as a CairnApiError. Shapes track
// internal/api/gen/openapi/cairnos.swagger.json by hand; see api/types.ts.

/** createSession opens a conversation. An empty personality uses the server
 * default; we omit the field entirely rather than send an empty string. */
export function createSession(personality?: string): Promise<CreateSessionResponse> {
  const trimmed = personality?.trim();
  const body = trimmed ? { personality: trimmed } : {};
  return request<CreateSessionResponse>('/chat/sessions', { method: 'POST', body });
}

/** listSessions lists conversations, newest activity first per the gateway. */
export function listSessions(limit?: number): Promise<ListSessionsResponse> {
  return request<ListSessionsResponse>('/chat/sessions', { query: { limit } });
}

/** listMessages returns the stored transcript for one session. */
export function listMessages(sessionId: string): Promise<ListMessagesResponse> {
  return request<ListMessagesResponse>(`/chat/sessions/${encodeURIComponent(sessionId)}/messages`);
}

/**
 * sendMessage posts a user turn and yields each ChatEvent from the gateway's
 * NDJSON stream in order. The body field is `message` and the session id rides
 * in the path, matching the proto3 JSON mapping in the swagger.
 */
export function sendMessage(
  sessionId: string,
  message: string,
  signal?: AbortSignal,
): AsyncGenerator<ChatEvent, void, void> {
  return streamChat(
    `/v1/chat/sessions/${encodeURIComponent(sessionId)}/messages`,
    { message },
    signal,
  );
}

/**
 * confirmAction resolves a pending WorkflowProposal. approve=true fires the
 * workflow; approve=false clears the gate without running. Either way the
 * gateway streams the follow-on ChatEvents (run status, the final reply), so a
 * caller folds them in the same way as sendMessage.
 */
export function confirmAction(
  sessionId: string,
  approve: boolean,
  signal?: AbortSignal,
): AsyncGenerator<ChatEvent, void, void> {
  return streamChat(
    `/v1/chat/sessions/${encodeURIComponent(sessionId)}/confirm`,
    { approve },
    signal,
  );
}

/** streamChat POSTs a body and yields ChatEvents from the NDJSON response. It
 * holds the shared stream-consumer logic both chat streaming calls need. */
async function* streamChat(
  path: string,
  body: unknown,
  signal?: AbortSignal,
): AsyncGenerator<ChatEvent, void, void> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  const token = getToken();
  if (token) {
    headers.Authorization = `Bearer ${token}`;
  }

  const init: RequestInit = { method: 'POST', headers, body: JSON.stringify(body) };
  if (signal) {
    init.signal = signal;
  }

  const response = await fetch(path, init);

  if (!response.ok) {
    if (response.status === 401) {
      clearToken();
    }
    let message = `chat request failed with status ${response.status}`;
    try {
      const errBody = (await response.json()) as { error?: { message?: string } };
      if (errBody.error?.message) {
        message = errBody.error.message;
      }
    } catch {
      // Non-JSON error body; the status-derived message stands.
    }
    throw new CairnApiError({ code: 'gateway.error', message }, response.status);
  }

  if (!response.body) {
    throw new CairnApiError(
      { code: 'gateway.no_stream', message: 'the gateway returned no response body' },
      response.status,
    );
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';

  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }

      buffer += decoder.decode(value, { stream: true });

      let newline = buffer.indexOf('\n');
      while (newline !== -1) {
        const line = buffer.slice(0, newline).trim();
        buffer = buffer.slice(newline + 1);
        if (line.length > 0) {
          yield parseLine(line);
        }
        newline = buffer.indexOf('\n');
      }
    }

    // A final line without a trailing newline is still a complete event.
    const tail = buffer.trim();
    if (tail.length > 0) {
      yield parseLine(tail);
    }
  } finally {
    reader.releaseLock();
  }
}

function parseLine(line: string): ChatEvent {
  const envelope = JSON.parse(line) as StreamEnvelope<ChatEvent>;

  if (envelope.error) {
    throw new CairnApiError(envelope.error, 200);
  }

  if (!envelope.result) {
    throw new CairnApiError(
      { code: 'gateway.bad_stream_line', message: 'stream line had neither result nor error' },
      200,
    );
  }

  return envelope.result;
}
