import type { RunStreamState } from './useRunStream';
import { ErrorBanner } from '@/components/ErrorBanner';
import { StatusBadge } from './StatusBadge';

interface LiveRunPanelProps {
  state: RunStreamState;
  /** cancel aborts the in-flight run; the button shows only while running. */
  cancel: () => void;
}

/**
 * LiveRunPanel renders a run's live state: the run id and terminal status, any
 * stream error, and the per-action status table. Both the run monitor and the
 * builder's run panel mount it so the live view is defined once, not copied.
 */
export function LiveRunPanel({ state, cancel }: LiveRunPanelProps) {
  return (
    <div className="space-y-4">
      {state.runId || state.running ? (
        <p className="text-sm text-gray-600">
          {state.runId ? (
            <>
              Run <span className="font-mono">{state.runId}</span>
            </>
          ) : (
            'Starting run'
          )}
          {state.finalStatus ? (
            <>
              {' '}
              finished <StatusBadge status={state.finalStatus} />
            </>
          ) : null}
          {state.running ? (
            <button
              type="button"
              onClick={cancel}
              className="ml-3 rounded border border-gray-300 px-2 py-0.5 text-xs"
            >
              Cancel
            </button>
          ) : null}
        </p>
      ) : null}

      {state.error ? (
        <ErrorBanner
          error={Object.assign(new Error(state.error.message), { code: state.error.code })}
          title="Run failed"
        />
      ) : null}

      {state.actions.length > 0 ? (
        <table className="w-full border-collapse text-sm">
          <caption className="sr-only">Live action status</caption>
          <thead>
            <tr className="border-b border-gray-200 text-left text-gray-500">
              <th scope="col" className="py-1">
                Action
              </th>
              <th scope="col" className="py-1">
                Status
              </th>
              <th scope="col" className="py-1">
                Attempt
              </th>
            </tr>
          </thead>
          <tbody>
            {state.actions.map((action) => (
              <tr key={action.name} className="border-b border-gray-100">
                <td className="py-1 font-mono">{action.name}</td>
                <td className="py-1">
                  <StatusBadge status={action.status} />
                </td>
                <td className="py-1">{action.attempt}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : (
        <p className="text-sm text-gray-500">Trigger a run to watch action status stream in.</p>
      )}
    </div>
  );
}
