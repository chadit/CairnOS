import { StatusBadge } from '@/runs/StatusBadge';
import type { TranscriptRow } from './types';

interface TranscriptProps {
  rows: TranscriptRow[];
  /** streaming shows a quiet "assistant is replying" line under the transcript. */
  streaming: boolean;
}

/**
 * Transcript renders the ordered chat rows. User and assistant rows are bubbles;
 * a tool row is a subtle "used <tool>" note; a run row shows a status badge.
 * The list is a log region so a screen reader announces new rows as they arrive.
 */
export function Transcript({ rows, streaming }: TranscriptProps) {
  if (rows.length === 0 && !streaming) {
    return <p className="text-sm text-gray-500">No messages yet. Say hello to get started.</p>;
  }

  return (
    <div className="space-y-3" role="log" aria-label="Conversation" aria-live="polite">
      {rows.map((row) => (
        <Row key={row.id} row={row} />
      ))}
      {streaming ? (
        <p className="text-xs text-gray-500" role="status">
          Assistant is replying...
        </p>
      ) : null}
    </div>
  );
}

function Row({ row }: { row: TranscriptRow }) {
  switch (row.kind) {
    case 'user':
      return (
        <div className="flex justify-end">
          <div className="max-w-[80%] rounded-lg bg-gray-900 px-3 py-2 text-sm text-white">
            {row.text}
          </div>
        </div>
      );
    case 'assistant':
      return (
        <div className="flex justify-start">
          <div className="max-w-[80%] whitespace-pre-wrap rounded-lg bg-white px-3 py-2 text-sm text-gray-900 ring-1 ring-gray-200">
            {row.text}
          </div>
        </div>
      );
    case 'tool':
      return (
        <p className="text-xs text-gray-500">
          used <span className="font-mono">{row.tool}</span>
        </p>
      );
    case 'run':
      return (
        <p className="text-xs text-gray-600">
          run <span className="font-mono">{row.runId || 'pending'}</span>{' '}
          <StatusBadge status={row.status} />
        </p>
      );
    default:
      return null;
  }
}
