import { Link } from 'react-router-dom';

import { useRuns } from '@/api/hooks';
import { ErrorBanner } from '@/components/ErrorBanner';
import { Spinner } from '@/components/Spinner';
import { StatusBadge } from '@/runs/StatusBadge';

/** RunsListPage lists recent runs and links each to its detail. */
export function RunsListPage() {
  const { data, isLoading, isError, error } = useRuns();

  if (isLoading) {
    return <Spinner label="Loading runs" />;
  }
  if (isError) {
    return <ErrorBanner error={error} title="Could not load runs" />;
  }

  const runs = data?.runs ?? [];

  return (
    <div className="space-y-4">
      <h1 className="text-xl font-semibold">Runs</h1>
      {runs.length === 0 ? (
        <p className="text-sm text-gray-500">No runs yet. Trigger one from the run monitor.</p>
      ) : (
        <table className="w-full border-collapse text-sm">
          <caption className="sr-only">Recent runs</caption>
          <thead>
            <tr className="border-b border-gray-200 text-left text-gray-500">
              <th scope="col" className="py-1">
                Run
              </th>
              <th scope="col" className="py-1">
                Workflow
              </th>
              <th scope="col" className="py-1">
                Status
              </th>
              <th scope="col" className="py-1">
                Started
              </th>
            </tr>
          </thead>
          <tbody>
            {runs.map((run) => (
              <tr key={run.runId} className="border-b border-gray-100">
                <td className="py-1 font-mono">
                  <Link to={`/runs/${encodeURIComponent(run.runId)}`} className="underline">
                    {run.runId}
                  </Link>
                </td>
                <td className="py-1">{run.workflowName}</td>
                <td className="py-1">
                  <StatusBadge status={run.status} />
                </td>
                <td className="py-1 text-gray-500">{run.startedAt ?? run.createdAt ?? '-'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
