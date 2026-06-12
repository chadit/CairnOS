import { type FormEvent, useState } from 'react';

import { ErrorBanner } from '@/components/ErrorBanner';
import { useAuth } from '@/auth/context';

/**
 * LoginPage exchanges an access key for a token. It works whether or not the
 * server enforces auth: with auth off, the operator can skip it and still use
 * the app, since Health and the read calls answer without a token.
 */
export function LoginPage({ onSkip }: { onSkip?: () => void }) {
  const { login } = useAuth();
  const [key, setKey] = useState('');
  const [error, setError] = useState<unknown>(null);
  const [submitting, setSubmitting] = useState(false);

  function handleSubmit(event: FormEvent) {
    event.preventDefault();
    setError(null);
    setSubmitting(true);
    login(key.trim())
      .catch((err: unknown) => setError(err))
      .finally(() => setSubmitting(false));
  }

  return (
    <main className="mx-auto mt-24 max-w-sm px-4">
      <h1 className="mb-2 text-2xl font-semibold">CairnOS Builder</h1>
      <p className="mb-6 text-sm text-gray-600">
        Sign in with an access key. If this server runs with auth off, you can skip sign-in.
      </p>

      <form onSubmit={handleSubmit} className="space-y-4">
        <div>
          <label htmlFor="access-key" className="block text-sm font-medium">
            Access key
          </label>
          <input
            id="access-key"
            name="access-key"
            type="password"
            autoComplete="current-password"
            value={key}
            onChange={(event) => setKey(event.target.value)}
            className="mt-1 w-full rounded border border-gray-300 px-3 py-2 text-sm"
          />
        </div>

        {error ? <ErrorBanner error={error} title="Sign-in failed" /> : null}

        <button
          type="submit"
          disabled={submitting || key.trim().length === 0}
          className="w-full rounded bg-gray-900 px-3 py-2 text-sm font-medium text-white disabled:opacity-50"
        >
          {submitting ? 'Signing in...' : 'Sign in'}
        </button>
      </form>

      {onSkip ? (
        <button
          type="button"
          onClick={onSkip}
          className="mt-4 w-full text-sm text-gray-500 underline"
        >
          Continue without signing in
        </button>
      ) : null}
    </main>
  );
}
