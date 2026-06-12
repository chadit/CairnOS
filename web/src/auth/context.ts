import { createContext, useContext } from 'react';

/** AuthState is the auth surface the shell and pages read. */
export interface AuthState {
  /** authed is true once a token is held. With auth off, a user can still
   * sign in, but Health and the read calls work without it. */
  authed: boolean;
  login: (key: string) => Promise<void>;
  logout: () => void;
}

export const AuthContext = createContext<AuthState | null>(null);

/** useAuth reads the auth state; throws if used outside the provider. */
export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error('useAuth must be used within an AuthProvider');
  }
  return ctx;
}
