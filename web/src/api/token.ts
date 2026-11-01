// Token storage for the bearer credential. The token lives in a module-level
// variable so calls within a session never touch storage, and mirrors to
// localStorage so a refresh keeps the user signed in. Auth is optional in
// CairnOS: when the server runs with auth off, no token is ever set and calls
// still work.

const STORAGE_KEY = 'cairnos.token';

let memoryToken: string | null = null;

/** loadToken seeds the in-memory token from localStorage on app start. */
export function loadToken(): string | null {
  if (memoryToken !== null) {
    return memoryToken;
  }

  try {
    memoryToken = window.localStorage.getItem(STORAGE_KEY);
  } catch {
    // Private-mode browsers throw on storage access; an in-memory token still
    // works for the session, so a failed read is not fatal.
    memoryToken = null;
  }

  return memoryToken;
}

/** getToken returns the current bearer token, or null when unauthenticated. */
export function getToken(): string | null {
  return memoryToken ?? loadToken();
}

/** setToken stores a freshly minted token in memory and localStorage. */
export function setToken(token: string): void {
  memoryToken = token;

  try {
    window.localStorage.setItem(STORAGE_KEY, token);
  } catch {
    // See loadToken: storage may be unavailable; the session token still holds.
  }
}

/** clearToken drops the credential on logout or after a 401. */
export function clearToken(): void {
  memoryToken = null;

  try {
    window.localStorage.removeItem(STORAGE_KEY);
  } catch {
    // Nothing to recover from; the in-memory token is already gone.
  }
}
