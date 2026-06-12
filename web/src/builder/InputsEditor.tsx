import { useState } from 'react';

import { type InputType } from '@/workflow/model';
import { useBuilderStore } from './store';

const TYPES: InputType[] = ['string', 'int', 'float', 'bool', 'string[]'];

/** InputsEditor declares the workflow's run inputs (name, type, required). */
export function InputsEditor() {
  const inputs = useBuilderStore((state) => state.meta.inputs);
  const setInput = useBuilderStore((state) => state.setInput);
  const removeInput = useBuilderStore((state) => state.removeInput);
  const [newName, setNewName] = useState('');

  function addInput() {
    const name = newName.trim();
    if (!name || inputs[name]) {
      return;
    }
    setInput(name, { type: 'string', required: false, hasDefault: false, description: '' });
    setNewName('');
  }

  return (
    <section className="space-y-2">
      <h3 className="text-sm font-semibold">Workflow inputs</h3>
      <ul className="space-y-2">
        {Object.entries(inputs).map(([name, decl]) => (
          <li key={name} className="flex flex-wrap items-center gap-2 text-sm">
            <span className="font-mono">{name}</span>
            <label className="sr-only" htmlFor={`type-${name}`}>
              Type for {name}
            </label>
            <select
              id={`type-${name}`}
              value={decl.type}
              onChange={(event) =>
                setInput(name, { ...decl, type: event.target.value as InputType })
              }
              className="rounded border border-gray-300 px-1 py-0.5"
            >
              {TYPES.map((type) => (
                <option key={type} value={type}>
                  {type}
                </option>
              ))}
            </select>
            <label className="flex items-center gap-1">
              <input
                type="checkbox"
                checked={decl.required}
                onChange={(event) => setInput(name, { ...decl, required: event.target.checked })}
              />
              required
            </label>
            <button
              type="button"
              onClick={() => removeInput(name)}
              className="text-xs text-red-600 underline"
            >
              remove
            </button>
          </li>
        ))}
      </ul>
      <div className="flex gap-2">
        <label className="sr-only" htmlFor="new-input">
          New input name
        </label>
        <input
          id="new-input"
          value={newName}
          onChange={(event) => setNewName(event.target.value)}
          placeholder="input name"
          className="rounded border border-gray-300 px-2 py-1 text-sm"
        />
        <button type="button" onClick={addInput} className="rounded bg-gray-200 px-2 py-1 text-sm">
          Add input
        </button>
      </div>
    </section>
  );
}
