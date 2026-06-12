import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';

import { onUnauthorized } from '@/api/client';
import { login as loginRequest } from '@/api/endpoints';
import { clearToken, getToken } from '@/api/token';
import { AuthContext, type AuthState } from './context';

/** AuthProvider holds the session flag and wires a 401 to a forced sign-out. */
export function AuthProvider({ children }: { children: ReactNode }) {
  const [authed, setAuthed] = useState<boolean>(() => getToken() !== null);

  const logout = useCallback(() => {
    clearToken();
    setAuthed(false);
  }, []);

  const login = useCallback(async (key: string) => {
    await loginRequest(key);
    setAuthed(true);
  }, []);

  // A 401 from any guarded call drops the session so the shell falls back to
  // login. The client already cleared the token by the time this fires.
  useEffect(() => {
    onUnauthorized(() => setAuthed(false));
    return () => onUnauthorized(() => undefined);
  }, []);

  const value = useMemo<AuthState>(() => ({ authed, login, logout }), [authed, login, logout]);

  return <AuthContext value={value}>{children}</AuthContext>;
}
