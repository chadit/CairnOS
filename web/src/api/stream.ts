import type { RunEvent } from './types';
import { base64Encode } from './endpoints';
import { openNdjsonStream, readNdjson } from './ndjson';

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
 * newline-delimited JSON stream in order. Each line is a {result?, error?}
 * envelope; a line carrying `error` aborts the stream by throwing a
 * CairnApiError, matching the unary client's failure shape.
 */
export async function* streamRunEvents(
  workflowName: string,
  inputs: Record<string, string>,
  options?: RunStreamOptions,
  signal?: AbortSignal,
): AsyncGenerator<RunEvent, void, void> {
  const body: { inputs: Record<string, string>; toml?: string } = { inputs };
  if (options?.toml !== undefined) {
    // grpc-gateway maps the proto `bytes` field as base64 in JSON, the same
    // encoding saveWorkflow and validateWorkflow use.
    body.toml = base64Encode(options.toml);
  }

  const stream = await openNdjsonStream(
    `/v1/workflows/${encodeURIComponent(workflowName)}/runs`,
    body,
    'run',
    signal,
  );
  yield* readNdjson<RunEvent>(stream);
}
