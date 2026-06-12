import type { WorkflowProposal } from '@/api/types';

interface ConfirmationCardProps {
  proposal: WorkflowProposal;
  /** busy disables both buttons while a confirm stream is in flight. */
  busy: boolean;
  onApprove: () => void;
  onDeny: () => void;
}

/** formatBudget shows the estimated spend, or a placeholder when the server
 * sent none, so the user always sees what they are signing off on. */
function formatBudget(value: number | undefined): string {
  if (value === undefined) {
    return 'not estimated';
  }
  return `$${value.toFixed(2)}`;
}

/**
 * ConfirmationCard renders a parked WorkflowProposal with Approve and Deny
 * buttons. It is purely presentational: it shows the workflow name, the inputs,
 * and the estimated budget, and reports the user's click. It never runs a
 * workflow on its own. The two buttons are real <button> elements with explicit
 * labels so a keyboard or screen-reader user reaches the same decision.
 */
export function ConfirmationCard({ proposal, busy, onApprove, onDeny }: ConfirmationCardProps) {
  const inputEntries = Object.entries(proposal.inputs ?? {});

  return (
    <section
      role="group"
      aria-labelledby="confirm-proposal-heading"
      className="rounded border border-amber-300 bg-amber-50 p-4"
    >
      <h2 id="confirm-proposal-heading" className="text-sm font-semibold text-amber-900">
        Confirm before running
      </h2>
      <p className="mt-1 text-sm text-amber-900">
        The assistant wants to run a workflow. It will not start until you approve.
      </p>

      <dl className="mt-3 space-y-2 text-sm">
        <div className="flex gap-2">
          <dt className="font-medium text-gray-700">Workflow</dt>
          <dd className="font-mono">{proposal.workflowName}</dd>
        </div>
        <div className="flex gap-2">
          <dt className="font-medium text-gray-700">Estimated budget</dt>
          <dd className="font-mono">{formatBudget(proposal.estimatedBudgetUsd)}</dd>
        </div>
        <div>
          <dt className="font-medium text-gray-700">Inputs</dt>
          <dd className="mt-1">
            {inputEntries.length === 0 ? (
              <span className="text-gray-500">none</span>
            ) : (
              <ul className="space-y-0.5 font-mono text-xs">
                {inputEntries.map(([key, value]) => (
                  <li key={key}>
                    <span className="text-gray-600">{key}</span>
                    {' = '}
                    <span>{JSON.stringify(value)}</span>
                  </li>
                ))}
              </ul>
            )}
          </dd>
        </div>
      </dl>

      <div className="mt-4 flex gap-2">
        <button
          type="button"
          onClick={onApprove}
          disabled={busy}
          className="rounded bg-amber-700 px-3 py-1 text-sm font-medium text-white disabled:opacity-50"
        >
          Approve and run
        </button>
        <button
          type="button"
          onClick={onDeny}
          disabled={busy}
          className="rounded border border-gray-300 bg-white px-3 py-1 text-sm disabled:opacity-50"
        >
          Deny
        </button>
      </div>
    </section>
  );
}
