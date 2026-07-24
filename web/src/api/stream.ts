import type { RunEvent, StreamEnvelope } from './types';
import { CairnApiError } from './client';
import { base64Encode } from './endpoints';
import { clearToken, getToken } from './token';

/**
 * RunStreamOptions carries an inline test definition. When `toml` is set the
 * gateway runs those bytes directly instead of resolving the workflow by name,
 * the builder's single-action test path.
 */
export interface RunStreamOptions {
  toml?: string;
}

/**
 * streamRunEvents starts a run and yields each RunEvent from the gateway's
 * newline-delimited JSON stream in order (.agents/spec/07-api-protocols.md,
 * "Streaming"). Each line is a {result?, error?} envelope; a line carrying
 * `error` aborts the stream by throwing a CairnApiError, matching the unary
 * client's failure shape.
 */
export async function* streamRunEvents(
  workflowName: string,
  inputs: Record<string, string>,
  options?: RunStreamOptions,
  signal?: AbortSignal,
): AsyncGenerator<RunEvent, void, void> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  const token = getToken();
  if (token) {
    headers.Authorization = `Bearer ${token}`;
  }

  const body: { inputs: Record<string, string>; toml?: string } = { inputs };
  if (options?.toml !== undefined) {
    // grpc-gateway maps the proto `bytes` field as base64 in JSON, the same
    // encoding saveWorkflow and validateWorkflow use.
    body.toml = base64Encode(options.toml);
  }

  const init: RequestInit = {
    method: 'POST',
    headers,
    body: JSON.stringify(body),
  };
  if (signal) {
    init.signal = signal;
  }

  const response = await fetch(`/v1/workflows/${encodeURIComponent(workflowName)}/runs`, init);

  if (!response.ok) {
    if (response.status === 401) {
      clearToken();
    }
    let message = `run failed with status ${response.status}`;
    try {
      const body = (await response.json()) as { error?: { message?: string } };
      if (body.error?.message) {
        message = body.error.message;
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

function parseLine(line: string): RunEvent {
  const envelope = JSON.parse(line) as StreamEnvelope<RunEvent>;

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
