import { HttpResponse } from 'msw';

// Mocked NDJSON stream bodies and the envelope lines that go in them. The
// gateway streams one {result|error} envelope per line, so every test that
// exercises a run or a chat turn needs the same two pieces: a Response carrying
// an NDJSON body, and lines in the shapes
// internal/api/gen/openapi/cairnos.swagger.json documents.

/** ndjsonResponse serves the lines as a single NDJSON body, the cheap shape for
 * component tests that only care about the events. */
export function ndjsonResponse(lines: string[]): Response {
  return new HttpResponse(lines.join('\n') + '\n', {
    headers: { 'Content-Type': 'application/x-ndjson' },
  });
}

/** chunkedNdjsonResponse splits the body in half across two chunks, so a client
 * test exercises the reader path rather than one JSON parse and proves a line
 * spanning a chunk boundary is reassembled. */
export function chunkedNdjsonResponse(lines: string[]): Response {
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

/** resultLine wraps one event payload in the success envelope. */
export function resultLine(result: unknown): string {
  return JSON.stringify({ result });
}

/** errorLine builds the envelope that aborts a stream mid-flight. */
export function errorLine(code: string, message: string): string {
  return JSON.stringify({ error: { code, message } });
}

// Every run fixture belongs to the `hello` workflow. No test asserts on the
// name, so it stays out of the builder signatures below.
const fixtureWorkflow = 'hello';

/**
 * runEventLines builds a full run stream: the started event, one transition per
 * [action, status] pair in the order given, then the finished event. Repeating
 * an action name is how a fixture walks it from running to its terminal status.
 */
export function runEventLines(runId: string, transitions: [string, string][]): string[] {
  return [
    resultLine({ runId, type: 'RUN_EVENT_TYPE_RUN_STARTED' }),
    ...transitions.map(([name, status]) =>
      resultLine({ runId, type: 'RUN_EVENT_TYPE_ACTION_TRANSITION', action: { name, status } }),
    ),
    runFinishedLine(runId),
  ];
}

/** runFinishedLine is the terminal event on its own, for fixtures that only
 * need the run to complete. */
export function runFinishedLine(runId: string): string {
  return resultLine({
    runId,
    type: 'RUN_EVENT_TYPE_RUN_FINISHED',
    run: { runId, workflowName: fixtureWorkflow, status: 'succeeded' },
  });
}
