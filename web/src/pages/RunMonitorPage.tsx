import { type FormEvent, useState } from 'react';

import { LiveRunPanel } from '@/runs/LiveRunPanel';
import { parseInputs } from '@/runs/inputs';
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
    void start(workflow.trim(), parseInputs(inputsText));
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
      </form>

      <LiveRunPanel state={state} cancel={cancel} />
    </div>
  );
}
