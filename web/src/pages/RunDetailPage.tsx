import { useState } from 'react';
import { Link, useParams } from 'react-router-dom';

import { cancelRun } from '@/api/endpoints';
import { useRun } from '@/api/hooks';
import { ErrorBanner } from '@/components/ErrorBanner';
import { Spinner } from '@/components/Spinner';
import { StatusBadge } from '@/runs/StatusBadge';

/** RunDetailPage shows one run's state and its per-action result table. */
export function RunDetailPage() {
  const { runId } = useParams<{ runId: string }>();
  const { data, isLoading, isError, error, refetch } = useRun(runId);
  const [cancelError, setCancelError] = useState<unknown>(null);

  if (isLoading) {
    return <Spinner label="Loading run" />;
  }
  if (isError) {
    return <ErrorBanner error={error} title="Could not load run" />;
  }
  if (!data) {
    return <p className="text-sm text-gray-500">Run not found.</p>;
  }

  const { run, actions = [] } = data;
  const active = run.status === 'running' || run.status === 'pending';

  async function handleCancel() {
    if (!runId) {
      return;
    }
    setCancelError(null);
    try {
      await cancelRun(runId);
      void refetch();
    } catch (err) {
      setCancelError(err);
    }
  }

  return (
    <div className="space-y-4">
      <Link to="/runs" className="text-sm text-gray-500 underline">
        Back to runs
      </Link>
      <div className="flex items-center gap-3">
        <h1 className="text-xl font-semibold">{run.workflowName}</h1>
        <StatusBadge status={run.status} />
        {active ? (
          <button
            type="button"
            onClick={() => void handleCancel()}
            className="rounded border border-gray-300 px-3 py-1 text-sm"
          >
            Cancel run
          </button>
        ) : null}
      </div>
      <p className="font-mono text-xs text-gray-500">{run.runId}</p>

      {run.error ? <ErrorBanner error={toError(run.error)} title="Run error" /> : null}
      {cancelError ? <ErrorBanner error={cancelError} title="Cancel failed" /> : null}

      <table className="w-full border-collapse text-sm">
        <caption className="sr-only">Action results</caption>
        <thead>
          <tr className="border-b border-gray-200 text-left text-gray-500">
            <th scope="col" className="py-1">
              Action
            </th>
            <th scope="col" className="py-1">
              Status
            </th>
            <th scope="col" className="py-1">
              Runner
            </th>
            <th scope="col" className="py-1">
              Cost (USD)
            </th>
          </tr>
        </thead>
        <tbody>
          {actions.map((action) => (
            <tr key={action.name} className="border-b border-gray-100">
              <td className="py-1 font-mono">{action.name}</td>
              <td className="py-1">
                <StatusBadge status={action.status} />
              </td>
              <td className="py-1">{action.runner ?? '-'}</td>
              <td className="py-1">{formatCost(action.usage?.costUsd)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function formatCost(cost: number | undefined): string {
  if (cost === undefined) {
    return '-';
  }
  return `$${cost.toFixed(4)}`;
}

function toError(apiError: { code: string; message: string }): Error {
  return Object.assign(new Error(apiError.message), { code: apiError.code });
}
