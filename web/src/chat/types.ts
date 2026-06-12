import type { WorkflowProposal } from '@/api/types';

/**
 * TranscriptRow is one rendered line in the chat view. It is the view model the
 * stream folder builds; the API ChatMessage and ChatEvent shapes are mapped
 * into these so the transcript renders user text, assistant text, the subtle
 * "used <tool>" notes, and run status from one ordered list.
 */
export type TranscriptRow =
  | { kind: 'user'; id: string; text: string }
  | { kind: 'assistant'; id: string; text: string }
  | { kind: 'tool'; id: string; tool: string }
  | { kind: 'run'; id: string; runId: string; status: string };

/**
 * PendingProposal is a confirmation gate the UI is holding. It exists only when
 * a CONFIRMATION_REQUIRED event has arrived and the user has not yet answered.
 * Holding it as state (rather than acting on it) is what stops a stray proposal
 * from auto-running: nothing fires until the user clicks Approve or Deny.
 */
export interface PendingProposal {
  proposal: WorkflowProposal;
}
