import '@testing-library/jest-dom/vitest';
import { afterAll, afterEach, beforeAll } from 'vitest';

import { server } from './src/test/server';

// jsdom replaces AbortController/AbortSignal with its own, but Node's fetch
// (which MSW intercepts) validates a passed signal against Node's AbortSignal
// and rejects jsdom's. The two come from different module contexts, so any
// component that passes an abort signal to fetch throws "Expected signal to be
// an instance of AbortSignal" under the test environment only. A browser
// never hits this; it is purely a jsdom-vs-Node primitive mismatch.
//
// The fix wraps fetch so the jsdom-built signal is dropped before the request
// reaches Node's validation. The stream module's abort behavior is covered by
// its own unit tests, which call it directly without jsdom, so dropping the
// signal here costs no real coverage. The wrap happens after server.listen so
// it sits in front of MSW's interceptor rather than behind it.
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
beforeAll(() => {
  server.listen({ onUnhandledRequest: 'error' });
  dropSignal();
});

afterEach(() => {
  server.resetHandlers();
});

afterAll(() => {
  server.close();
});
