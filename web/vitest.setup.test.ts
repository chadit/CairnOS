import http from 'node:http';
import type { AddressInfo } from 'node:net';

import { afterAll, beforeAll, describe, expect, it } from 'vitest';

// A real local server stands in for the network. A URL that refuses the
// connection would reject under either MSW setting, so only a server that
// answers can tell "MSW blocked it" apart from "it went out and failed".
let networkHits = 0;
const realServer = http.createServer((_req, res) => {
  networkHits += 1;
  res.end('real network');
});
let unhandledURL = '';

beforeAll(async () => {
  await new Promise<void>((resolve) => realServer.listen(0, '127.0.0.1', () => resolve()));
  const { port } = realServer.address() as AddressInfo;
  unhandledURL = `http://127.0.0.1:${port}/no-handler`;
});

afterAll(async () => {
  await new Promise<void>((resolve) => realServer.close(() => resolve()));
});

describe('shared MSW server', () => {
  it('rejects a request no handler matches before it reaches the network', async () => {
    await expect(fetch(unhandledURL)).rejects.toThrow('fetch failed');
    expect(networkHits).toBe(0);
  });
});
