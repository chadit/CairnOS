import { useState } from 'react';

import type { JsonValue } from '@/api/types';
import { useBuilderStore } from './store';

/**
 * Inspector edits the selected action. Task actions expose runner and params;
 * agent actions expose driver, goal, tools, and budget. Params and tools are
 * free-form, so they are edited as text and parsed on change; a parse error is
 * surfaced inline rather than silently dropping the edit.
 */
export function Inspector() {
  const selectedId = useBuilderStore((state) => state.selectedId);
  const node = useBuilderStore((state) => state.nodes.find((item) => item.id === selectedId));
  const updateAction = useBuilderStore((state) => state.updateAction);
  const removeAction = useBuilderStore((state) => state.removeAction);

  if (!node) {
    return (
      <aside className="w-80 shrink-0 border-l border-gray-200 p-4 text-sm text-gray-500">
        Select an action to edit it.
      </aside>
    );
  }

  const { action } = node.data;

  return (
    <aside className="w-80 shrink-0 space-y-4 border-l border-gray-200 p-4 text-sm">
      <div className="flex items-center justify-between">
        <h2 className="font-semibold">{action.name}</h2>
        <button
          type="button"
          onClick={() => removeAction(action.name)}
          className="text-xs text-red-600 underline"
        >
          Delete
        </button>
      </div>

      <Field label="Kind">
        <select
          value={action.kind}
          onChange={(event) =>
            updateAction(action.name, { kind: event.target.value as 'task' | 'agent' })
          }
          className="w-full rounded border border-gray-300 px-2 py-1"
        >
          <option value="task">task</option>
          <option value="agent">agent</option>
        </select>
      </Field>

      <Field label="Condition">
        <input
          value={action.condition ?? ''}
          onChange={(event) =>
            updateAction(action.name, { condition: event.target.value || undefined })
          }
          className="w-full rounded border border-gray-300 px-2 py-1"
          placeholder="optional, e.g. ${{ a.output }} != 'X'"
        />
      </Field>

      {action.kind === 'task' ? (
        <TaskFields actionName={action.name} runner={action.runner} params={action.params} />
      ) : (
        <AgentFields
          actionName={action.name}
          driver={action.driver}
          goal={action.goal}
          tools={action.tools}
          hasTools={action.hasTools}
          budgetUsd={action.budgetUsd}
        />
      )}
    </aside>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="block space-y-1">
      <span className="block font-medium text-gray-700">{label}</span>
      {children}
    </label>
  );
}

function TaskFields({
  actionName,
  runner,
  params,
}: {
  actionName: string;
  runner: string | undefined;
  params: Record<string, JsonValue> | undefined;
}) {
  const updateAction = useBuilderStore((state) => state.updateAction);
  return (
    <>
      <Field label="Runner">
        <input
          value={runner ?? ''}
          onChange={(event) =>
            updateAction(actionName, { runner: event.target.value || undefined })
          }
          className="w-full rounded border border-gray-300 px-2 py-1"
          placeholder="e.g. mock"
        />
      </Field>
      <JsonField
        key={actionName}
        label="Params (JSON)"
        value={params ?? {}}
        onValue={(value) => updateAction(actionName, { params: value })}
      />
    </>
  );
}

function AgentFields({
  actionName,
  driver,
  goal,
  tools,
  hasTools,
  budgetUsd,
}: {
  actionName: string;
  driver: string | undefined;
  goal: string | undefined;
  tools: string[] | undefined;
  hasTools: boolean;
  budgetUsd: number | undefined;
}) {
  const updateAction = useBuilderStore((state) => state.updateAction);
  return (
    <>
      <Field label="Driver">
        <input
          value={driver ?? ''}
          onChange={(event) =>
            updateAction(actionName, { driver: event.target.value || undefined })
          }
          className="w-full rounded border border-gray-300 px-2 py-1"
        />
      </Field>
      <Field label="Goal">
        <textarea
          value={goal ?? ''}
          onChange={(event) => updateAction(actionName, { goal: event.target.value || undefined })}
          rows={3}
          className="w-full rounded border border-gray-300 px-2 py-1"
        />
      </Field>
      <Field label="Tools (comma-separated)">
        <input
          value={hasTools ? (tools ?? []).join(', ') : ''}
          onChange={(event) => {
            const list = event.target.value
              .split(',')
              .map((tool) => tool.trim())
              .filter((tool) => tool.length > 0);
            updateAction(actionName, { tools: list, hasTools: true });
          }}
          className="w-full rounded border border-gray-300 px-2 py-1"
          placeholder="e.g. memory"
        />
      </Field>
      <Field label="Budget (USD)">
        <input
          type="number"
          step="0.01"
          min="0"
          value={budgetUsd ?? ''}
          onChange={(event) => {
            const next = event.target.value === '' ? undefined : Number(event.target.value);
            updateAction(actionName, { budgetUsd: next });
          }}
          className="w-full rounded border border-gray-300 px-2 py-1"
        />
      </Field>
    </>
  );
}

/** JsonField edits a JSON object as text, parsing on each change and showing a
 * parse error rather than discarding the keystroke. */
function JsonField({
  label,
  value,
  onValue,
}: {
  label: string;
  value: Record<string, JsonValue>;
  onValue: (value: Record<string, JsonValue>) => void;
}) {
  // The parent re-keys this field by action name, so a fresh mount seeds the
  // textarea from the selected action's params. No effect is needed to track
  // selection changes.
  const [text, setText] = useState(() => JSON.stringify(value, null, 2));
  const [error, setError] = useState<string | null>(null);

  return (
    <label className="block space-y-1">
      <span className="block font-medium text-gray-700">{label}</span>
      <textarea
        value={text}
        onChange={(event) => {
          const next = event.target.value;
          setText(next);
          try {
            const parsed = JSON.parse(next) as unknown;
            if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
              setError('params must be a JSON object');
              return;
            }
            setError(null);
            onValue(parsed as Record<string, JsonValue>);
          } catch {
            setError('invalid JSON');
          }
        }}
        rows={6}
        spellCheck={false}
        className="w-full rounded border border-gray-300 px-2 py-1 font-mono text-xs"
      />
      {error ? <span className="text-xs text-red-600">{error}</span> : null}
    </label>
  );
}
