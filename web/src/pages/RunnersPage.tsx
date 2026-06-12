import { useRunners } from '@/api/hooks';
import { useBuilderStore } from '@/builder/store';
import { ErrorBanner } from '@/components/ErrorBanner';
import { Spinner } from '@/components/Spinner';

/**
 * RunnersPage lists registered runners and the actions in the current builder
 * workflow that use each one. Usage is derived from the loaded workflow, since
 * the engine does not expose a runner-to-workflow index; the builder is the
 * only place the UI knows which actions reference which runner.
 */
export function RunnersPage() {
  const { data, isLoading, isError, error } = useRunners();
  const nodes = useBuilderStore((state) => state.nodes);
  const workflowName = useBuilderStore((state) => state.meta.name);

  if (isLoading) {
    return <Spinner label="Loading runners" />;
  }
  if (isError) {
    return <ErrorBanner error={error} title="Could not load runners" />;
  }

  const usage = new Map<string, string[]>();
  for (const node of nodes) {
    const action = node.data.action;
    const ref = action.kind === 'task' ? action.runner : action.driver;
    if (!ref) {
      continue;
    }
    usage.set(ref, [...(usage.get(ref) ?? []), action.name]);
  }

  const runners = data?.runners ?? [];

  return (
    <div className="space-y-4">
      <h1 className="text-xl font-semibold">Runners</h1>
      <p className="text-sm text-gray-500">
        Usage is read from the workflow open in the builder
        {workflowName ? ` ("${workflowName}")` : ''}. Load a workflow there to see assignments.
      </p>
      <ul className="space-y-2">
        {runners.map((runner) => {
          const actions = usage.get(runner.name) ?? [];
          return (
            <li key={runner.name} className="rounded border border-gray-200 bg-white p-3">
              <p className="font-medium">{runner.name}</p>
              {actions.length > 0 ? (
                <p className="text-sm text-gray-600">
                  used by: <span className="font-mono">{actions.join(', ')}</span>
                </p>
              ) : (
                <p className="text-sm text-gray-400">not used by the open workflow</p>
              )}
            </li>
          );
        })}
      </ul>
    </div>
  );
}
