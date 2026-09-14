import type {
  ChatEvent,
  CreateSessionResponse,
  ListMessagesResponse,
  ListSessionsResponse,
} from './types';
import { request } from './client';
import { openNdjsonStream, readNdjson } from './ndjson';

// ChatService client. The unary calls reuse the shared `request` (auth attach,
// error-envelope unwrap, 401 handling). The two streaming calls (SendMessage,
// ConfirmAction) hold a long-lived response, which ndjson.ts reads for them.
// Shapes track internal/api/gen/openapi/cairnos.swagger.json by hand; see
// api/types.ts.

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
 * holds the request shape both chat streaming calls need. */
async function* streamChat(
  path: string,
  body: unknown,
  signal?: AbortSignal,
): AsyncGenerator<ChatEvent, void, void> {
  const stream = await openNdjsonStream(path, body, 'chat request', signal);
  yield* readNdjson<ChatEvent>(stream);
}
