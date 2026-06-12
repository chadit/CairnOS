import { useState } from 'react';

import { Canvas } from '@/builder/Canvas';
import { Inspector } from '@/builder/Inspector';
import { InputsEditor } from '@/builder/InputsEditor';
import { TomlPanel } from '@/builder/TomlPanel';
import { useBuilderStore } from '@/builder/store';

/** BuilderPage is the drag-and-drop workflow editor: meta, canvas, inspector,
 * inputs, and the TOML bridge. */
export function BuilderPage() {
  const meta = useBuilderStore((state) => state.meta);
  const setMeta = useBuilderStore((state) => state.setMeta);
  const addAction = useBuilderStore((state) => state.addAction);
  const [newAction, setNewAction] = useState('');
  const [newKind, setNewKind] = useState<'task' | 'agent'>('task');

  function handleAdd() {
    const name = newAction.trim();
    if (name) {
      addAction(name, newKind);
      setNewAction('');
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-end gap-3">
        <label className="space-y-1 text-sm">
          <span className="block font-medium">Workflow name</span>
          <input
            value={meta.name}
            onChange={(event) => setMeta({ name: event.target.value })}
            className="rounded border border-gray-300 px-2 py-1"
          />
        </label>
        <label className="space-y-1 text-sm">
          <span className="block font-medium">Description</span>
          <input
            value={meta.description}
            onChange={(event) => setMeta({ description: event.target.value })}
            className="w-72 rounded border border-gray-300 px-2 py-1"
          />
        </label>
        <label className="space-y-1 text-sm">
          <span className="block font-medium">Schedule</span>
          <input
            value={meta.schedule ?? ''}
            onChange={(event) => setMeta({ schedule: event.target.value || undefined })}
            placeholder="@daily or cron"
            className="rounded border border-gray-300 px-2 py-1"
          />
        </label>
      </div>

      <div className="flex flex-wrap items-end gap-2">
        <label className="space-y-1 text-sm">
          <span className="block font-medium">New action</span>
          <input
            value={newAction}
            onChange={(event) => setNewAction(event.target.value)}
            placeholder="action name"
            className="rounded border border-gray-300 px-2 py-1"
          />
        </label>
        <label className="space-y-1 text-sm">
          <span className="sr-only">New action kind</span>
          <select
            value={newKind}
            onChange={(event) => setNewKind(event.target.value as 'task' | 'agent')}
            className="rounded border border-gray-300 px-2 py-1"
          >
            <option value="task">task</option>
            <option value="agent">agent</option>
          </select>
        </label>
        <button
          type="button"
          onClick={handleAdd}
          className="rounded bg-gray-900 px-3 py-1 text-sm text-white"
        >
          Add action
        </button>
      </div>

      <div className="flex gap-4">
        <div className="flex-1 space-y-4">
          <Canvas />
          <InputsEditor />
          <TomlPanel />
        </div>
        <Inspector />
      </div>
    </div>
  );
}
