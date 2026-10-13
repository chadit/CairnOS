import '@testing-library/jest-dom/vitest';
import { afterAll, afterEach, beforeAll } from 'vitest';

import { server } from './src/test/server';

// jsdom swaps in its own AbortSignal, but Node's fetch (which MSW intercepts)
// rejects a signal that is not Node's, so a component passing one throws
// "Expected signal to be an instance of AbortSignal" under jsdom only. The
// wrapper drops it before that check; the stream module's abort behavior is
// covered by unit tests that run outside jsdom.
function dropSignal(): void {
  const innerFetch = globalThis.fetch;
  globalThis.fetch = function patchedFetch(input: RequestInfo | URL, init?: RequestInit) {
    if (init && 'signal' in init) {
      const { signal: _dropped, ...rest } = init;
      return innerFetch(input, rest);
    }
    return innerFetch(input, init);
  };
}

// One MSW server backs every test. Tests override handlers per case; the
// reset after each keeps cases isolated. No request ever leaves the process.
// MSW 3 drops an unknown option key without a word and defaults to 'warn',
// which lets an unmatched request out, so vitest.setup.test.ts pins this.
beforeAll(() => {
  server.listen({ onUnhandledFrame: 'error' });
  // After listen, so the wrapper sits in front of MSW's interceptor.
  dropSignal();
});

afterEach(() => {
  server.resetHandlers();
});

afterAll(() => {
  server.close();
});
