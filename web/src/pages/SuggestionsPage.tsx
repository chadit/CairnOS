import { useApproveSuggestion, useRejectSuggestion, useSuggestions } from '@/api/hooks';
import type { Suggestion } from '@/api/types';
import { ErrorBanner } from '@/components/ErrorBanner';
import { Spinner } from '@/components/Spinner';

/**
 * SuggestionsPage lists tuning suggestions and lets an operator approve or
 * reject one. Approve returns a rendered annotation for the operator to apply
 * by hand; nothing is written to any workflow or config file by the server.
 */
export function SuggestionsPage() {
  const { data, isLoading, isError, error } = useSuggestions();

  if (isLoading) {
    return <Spinner label="Loading suggestions" />;
  }
  if (isError) {
    return <ErrorBanner error={error} title="Could not load suggestions" />;
  }

  const suggestions = data?.suggestions ?? [];

  return (
    <div className="space-y-4">
      <h1 className="text-xl font-semibold">Tuning suggestions</h1>
      {suggestions.length === 0 ? (
        <p className="text-sm text-gray-500">No suggestions yet.</p>
      ) : (
        <ul className="space-y-3">
          {suggestions.map((suggestion) => (
            <SuggestionCard key={suggestion.id} suggestion={suggestion} />
          ))}
        </ul>
      )}
    </div>
  );
}

function SuggestionCard({ suggestion }: { suggestion: Suggestion }) {
  const approve = useApproveSuggestion();
  const reject = useRejectSuggestion();

  const open = suggestion.status === 'open';
  const annotation = approve.data?.annotation;

  return (
    <li className="rounded border border-gray-200 bg-white p-4">
      <div className="flex flex-wrap items-center gap-2">
        <span className="rounded bg-gray-100 px-2 py-0.5 text-xs">{suggestion.kind}</span>
        <span className="font-mono text-sm">{suggestion.target}</span>
        <span className="ml-auto text-xs text-gray-500">{suggestion.status}</span>
      </div>
      {suggestion.rationale ? (
        <p className="mt-2 text-sm text-gray-700">{suggestion.rationale}</p>
      ) : null}

      {suggestion.proposed ? (
        <pre className="mt-2 overflow-x-auto rounded bg-gray-50 p-2 text-xs">
          {JSON.stringify(suggestion.proposed, null, 2)}
        </pre>
      ) : null}

      {open ? (
        <div className="mt-3 flex gap-2">
          <button
            type="button"
            onClick={() => approve.mutate(suggestion.id)}
            disabled={approve.isPending}
            className="rounded bg-gray-900 px-3 py-1 text-sm text-white disabled:opacity-50"
          >
            Approve
          </button>
          <button
            type="button"
            onClick={() => reject.mutate(suggestion.id)}
            disabled={reject.isPending}
            className="rounded border border-gray-300 px-3 py-1 text-sm disabled:opacity-50"
          >
            Reject
          </button>
        </div>
      ) : null}

      {annotation ? (
        <div className="mt-3">
          <p className="text-xs font-medium text-gray-600">Apply this change by hand:</p>
          <pre className="mt-1 overflow-x-auto rounded bg-green-50 p-2 text-xs">{annotation}</pre>
        </div>
      ) : null}

      {approve.isError ? <ErrorBanner error={approve.error} title="Approve failed" /> : null}
      {reject.isError ? <ErrorBanner error={reject.error} title="Reject failed" /> : null}
    </li>
  );
}
