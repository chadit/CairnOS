import type { ApiError } from './types';
import { clearToken, getToken } from './token';

/**
 * CairnApiError wraps the canonical engine error so every rejected promise in
 * the data layer carries the same {code, message, details} shape, no matter
 * whether the failure came from the error envelope, an HTTP status, or a
 * transport fault.
 */
export class CairnApiError extends Error {
  readonly code: string;
  readonly status: number;
  readonly details: ApiError['details'];

  constructor(error: ApiError, status: number) {
    super(error.message);
    this.name = 'CairnApiError';
    this.code = error.code;
    this.status = status;
    if (error.details !== undefined) {
      this.details = error.details;
    }
  }
}

/** isCairnApiError narrows an unknown catch value to the typed error. */
export function isCairnApiError(value: unknown): value is CairnApiError {
  return value instanceof CairnApiError;
}

/**
 * onUnauthorized lets the app react to a 401 (drop the token, route to login)
 * without the client importing router state. The app registers a handler at
 * startup.
 */
let unauthorizedHandler: (() => void) | null = null;

export function onUnauthorized(handler: () => void): void {
  unauthorizedHandler = handler;
}

/** The error envelope shape the custom gateway error handler emits. */
interface ErrorEnvelope {
  status?: string;
  error?: ApiError;
}

/** rpcStatus is grpc-gateway's default error body, a fallback envelope. */
interface RpcStatus {
  code?: number;
  message?: string;
}

interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'DELETE';
  body?: unknown;
  query?: Record<string, string | number | boolean | string[] | undefined>;
  signal?: AbortSignal;
}

const BASE = '/v1';

function buildUrl(path: string, query?: RequestOptions['query']): string {
  const url = `${BASE}${path}`;
  if (!query) {
    return url;
  }

  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined) {
      continue;
    }
    if (Array.isArray(value)) {
      for (const item of value) {
        params.append(key, item);
      }
      continue;
    }
    params.append(key, String(value));
  }

  const qs = params.toString();
  return qs ? `${url}?${qs}` : url;
}

function authHeaders(): HeadersInit {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  const token = getToken();
  if (token) {
    headers.Authorization = `Bearer ${token}`;
  }
  return headers;
}

/** parseError pulls a canonical ApiError out of whatever the server returned. */
function parseError(raw: unknown, status: number): ApiError {
  if (raw && typeof raw === 'object') {
    const envelope = raw as ErrorEnvelope;
    if (envelope.error && typeof envelope.error.code === 'string') {
      return envelope.error;
    }

    const rpc = raw as RpcStatus;
    if (typeof rpc.message === 'string') {
      // grpc-gateway's default error body, when the custom handler did not
      // run. Surface it with a generic code so the UI still has a message.
      return { code: 'gateway.error', message: rpc.message };
    }
  }

  return { code: 'gateway.error', message: `request failed with status ${status}` };
}

/**
 * request is the one HTTP entry point. It attaches auth, sends JSON, and on
 * any non-2xx unwraps the error envelope into a CairnApiError. A 401 also
 * fires the unauthorized handler so the shell can route to login.
 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const init: RequestInit = {
    method: options.method ?? 'GET',
    headers: authHeaders(),
  };
  if (options.body !== undefined) {
    init.body = JSON.stringify(options.body);
  }
  if (options.signal) {
    init.signal = options.signal;
  }

  const response = await fetch(buildUrl(path, options.query), init);

  if (!response.ok) {
    let raw: unknown = null;
    try {
      raw = await response.json();
    } catch {
      // A non-JSON error body (proxy outage, HTML 502) still becomes a typed
      // error below via the status-only fallback.
    }

    if (response.status === 401) {
      clearToken();
      unauthorizedHandler?.();
    }

    throw new CairnApiError(parseError(raw, response.status), response.status);
  }

  if (response.status === 204) {
    return undefined as T;
  }

  return (await response.json()) as T;
}
