import { type FormEvent, useState } from 'react';

import { ErrorBanner } from '@/components/ErrorBanner';
import { StatusBadge } from '@/runs/StatusBadge';
import { useRunStream } from '@/runs/useRunStream';

/**
 * RunMonitorPage triggers a run by workflow name and shows action status
 * transitions live as the NDJSON stream arrives. Inputs are entered as
 * key=value lines, the shape the gateway's RunWorkflow body expects.
 */
export function RunMonitorPage() {
  const { state, start, cancel } = useRunStream();
  const [workflow, setWorkflow] = useState('');
  const [inputsText, setInputsText] = useState('');

  function handleSubmit(event: FormEvent) {
    event.preventDefault();
    const inputs = parseInputs(inputsText);
    void start(workflow.trim(), inputs);
  }

  return (
    <div className="space-y-4">
      <h1 className="text-xl font-semibold">Run monitor</h1>

      <form onSubmit={handleSubmit} className="flex flex-wrap items-end gap-3">
        <label className="space-y-1 text-sm">
          <span className="block font-medium">Workflow name</span>
          <input
            value={workflow}
            onChange={(event) => setWorkflow(event.target.value)}
            placeholder="e.g. hello"
            className="rounded border border-gray-300 px-2 py-1"
          />
        </label>
        <label className="space-y-1 text-sm">
          <span className="block font-medium">Inputs (key=value per line)</span>
          <textarea
            value={inputsText}
            onChange={(event) => setInputsText(event.target.value)}
            rows={2}
            className="w-72 rounded border border-gray-300 px-2 py-1 font-mono text-xs"
          />
        </label>
        <button
          type="submit"
          disabled={state.running || workflow.trim().length === 0}
          className="rounded bg-gray-900 px-3 py-1 text-sm text-white disabled:opacity-50"
        >
          {state.running ? 'Running...' : 'Run workflow'}
        </button>
        {state.running ? (
          <button
            type="button"
            onClick={cancel}
            className="rounded border border-gray-300 px-3 py-1 text-sm"
          >
            Cancel
          </button>
        ) : null}
      </form>

      {state.runId ? (
        <p className="text-sm text-gray-600">
          Run <span className="font-mono">{state.runId}</span>
          {state.finalStatus ? (
            <>
              {' '}
              finished <StatusBadge status={state.finalStatus} />
            </>
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

/** parseInputs turns key=value lines into the inputs map. Blank lines and
 * lines without an `=` are ignored. */
function parseInputs(text: string): Record<string, string> {
  const inputs: Record<string, string> = {};
  for (const line of text.split('\n')) {
    const trimmed = line.trim();
    const eq = trimmed.indexOf('=');
    if (eq <= 0) {
      continue;
    }
    inputs[trimmed.slice(0, eq).trim()] = trimmed.slice(eq + 1).trim();
  }
  return inputs;
}
