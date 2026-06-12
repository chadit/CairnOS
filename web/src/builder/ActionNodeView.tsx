import { Handle, Position, type NodeProps } from 'reactflow';

import type { ActionNodeData } from '@/workflow/graph';

/**
 * ActionNodeView renders one action on the canvas. The top handle is the
 * dependency target (an upstream connects into it); the bottom handle is the
 * source (this action feeds a downstream one). A node with validation errors
 * gets a red border and lists the canonical codes inline.
 */
export function ActionNodeView({ data, selected }: NodeProps<ActionNodeData>) {
  const { action, errorCodes } = data;
  const hasErrors = errorCodes.length > 0;

  return (
    <div
      className={`min-w-44 rounded border-2 bg-white px-3 py-2 text-sm shadow-sm ${
        hasErrors ? 'border-red-500' : selected ? 'border-gray-900' : 'border-gray-300'
      }`}
    >
      <Handle type="target" position={Position.Top} />
      <div className="flex items-center justify-between gap-2">
        <span className="font-medium">{action.name}</span>
        <span className="rounded bg-gray-100 px-1 text-xs text-gray-600">{action.kind}</span>
      </div>
      <p className="mt-1 truncate text-xs text-gray-500">
        {action.kind === 'task' ? (action.runner ?? 'no runner') : (action.driver ?? 'no driver')}
      </p>
      {hasErrors ? (
        <ul className="mt-1 space-y-0.5">
          {errorCodes.map((code) => (
            <li key={code} className="font-mono text-xs text-red-600">
              {code}
            </li>
          ))}
        </ul>
      ) : null}
      <Handle type="source" position={Position.Bottom} />
    </div>
  );
}
