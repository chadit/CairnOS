import { setupServer } from 'msw/node';

import { handlers } from './handlers';

// The shared MSW server. vitest.setup.ts starts and stops it; individual
// tests call server.use(...) to override a route. No request reaches a real
// gateway.
export const server = setupServer(...handlers);
