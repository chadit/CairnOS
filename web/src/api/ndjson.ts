import type { StreamEnvelope } from './types';
import { CairnApiError } from './client';
import { clearToken, getToken } from './token';

// Both streaming endpoints (RunWorkflow, and chat's SendMessage/ConfirmAction)
// speak the same newline-delimited JSON protocol: one {result?, error?} envelope
// per line, an `error` line aborting the stream. This module holds that protocol
// once so chat.ts and stream.ts only describe what they send.

/**
 * openNdjsonStream POSTs a JSON body and returns the response body to read
 * events from. It attaches the bearer token, drops it on a 401, and turns both
 * start-up failures into a CairnApiError: a non-2xx status (carrying the
 * gateway's error message when the body has one) and a 2xx with no body, which
 * would otherwise leave the caller with nothing to read. `failureLabel` names
 * the call in the status-derived message, for example "run failed with status
 * 500".
 */
export async function openNdjsonStream(
  path: string,
  body: unknown,
  failureLabel: string,
  signal?: AbortSignal,
): Promise<ReadableStream<Uint8Array>> {
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
    throw new CairnApiError(
      { code: 'gateway.error', message: await failureMessage(response, failureLabel) },
      response.status,
    );
  }

  if (!response.body) {
    throw new CairnApiError(
      { code: 'gateway.no_stream', message: 'the gateway returned no response body' },
      response.status,
    );
  }

  return response.body;
}

/**
 * readNdjson yields one decoded payload per line. It reassembles lines across
 * chunk boundaries, since a read can end mid-line, and treats a trailing line
 * with no newline as a complete event.
 */
export async function* readNdjson<T>(
  stream: ReadableStream<Uint8Array>,
): AsyncGenerator<T, void, void> {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let buffer = '';

  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }

      buffer += decoder.decode(value, { stream: true });

      // The last piece has no newline after it yet, so it waits for the next read.
      const lines = buffer.split('\n');
      buffer = lines.pop() ?? '';
      yield* parseLines<T>(lines);
    }

    yield* parseLines<T>([buffer]);
  } finally {
    reader.releaseLock();
  }
}

/** parseLines unwraps each line in order, skipping blank lines because they
 * carry no envelope. */
function* parseLines<T>(lines: string[]): Generator<T, void, void> {
  for (const raw of lines) {
    const line = raw.trim();
    if (line.length > 0) {
      yield parseEnvelope<T>(line);
    }
  }
}

/** parseEnvelope unwraps one stream line. An `error` envelope throws so the
 * failure reaches the caller in the same shape the unary client uses. */
function parseEnvelope<T>(line: string): T {
  const envelope = JSON.parse(line) as StreamEnvelope<T>;

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

/** failureMessage prefers the gateway's error text and falls back to the
 * status when the body is not the JSON error envelope. */
async function failureMessage(response: Response, failureLabel: string): Promise<string> {
  try {
    const body = (await response.json()) as { error?: { message?: string } };
    if (body.error?.message) {
      return body.error.message;
    }
  } catch {
    // Non-JSON error body; the status-derived message stands.
  }
  return `${failureLabel} failed with status ${response.status}`;
}
