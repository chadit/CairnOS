import { isCairnApiError } from '@/api/client';

interface ErrorBannerProps {
  error: unknown;
  /** title overrides the default heading when the context needs a clearer one. */
  title?: string;
}

/** codeOf pulls a canonical dotted code off any error carrier: a real
 * CairnApiError, or a plain object that carries a string `code` (the shape the
 * run stream surfaces). */
function codeOf(error: unknown): string | undefined {
  if (isCairnApiError(error)) {
    return error.code;
  }
  if (error && typeof error === 'object' && 'code' in error) {
    const { code } = error;
    return typeof code === 'string' ? code : undefined;
  }
  return undefined;
}

/**
 * ErrorBanner renders any thrown value as the canonical {code, message}. The
 * dotted code shows when present so an operator can match it to the engine
 * error model; otherwise the message stands alone.
 */
export function ErrorBanner({ error, title = 'Something went wrong' }: ErrorBannerProps) {
  const code = codeOf(error);
  const message = error instanceof Error ? error.message : String(error);

  return (
    <div role="alert" className="rounded border border-red-300 bg-red-50 p-3 text-sm text-red-800">
      <p className="font-semibold">{title}</p>
      {code ? <p className="font-mono text-xs text-red-600">{code}</p> : null}
      <p>{message}</p>
    </div>
  );
}
