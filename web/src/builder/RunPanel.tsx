import { useState } from 'react';

import type { JsonValue } from '@/api/types';
import type { Workflow } from '@/workflow/model';
import { saveWorkflow, validateWorkflow } from '@/api/endpoints';
import { ErrorBanner } from '@/components/ErrorBanner';
import { LiveRunPanel } from '@/runs/LiveRunPanel';
import { parseInputs } from '@/runs/inputs';
import { useRunStream } from '@/runs/useRunStream';
import { emitWorkflow } from '@/workflow/emit';
import { useBuilderStore } from './store';
import { buildTestWorkflow } from './testAction';
import { errorCodesByAction } from './validationErrors';

/**
 * RunPanel is the builder's save/run/test surface on top of the canvas. Save
 * validates the emitted TOML, marks any offending nodes, then writes it through
 * the version-store-backed PUT. Run streams the saved workflow live. Test runs
 * only the selected action, inline, without touching the workflows directory.
 */
export function RunPanel() {
  const meta = useBuilderStore((state) => state.meta);
  const currentWorkflow = useBuilderStore((state) => state.currentWorkflow);
  const selectedId = useBuilderStore((state) => state.selectedId);
  const setErrorCodes = useBuilderStore((state) => state.setErrorCodes);

  const { state, start, cancel } = useRunStream();

  const [inputsText, setInputsText] = useState(() => defaultInputsText(meta.inputs));
  const [saving, setSaving] = useState(false);
  const [saveNote, setSaveNote] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<unknown>(null);

  async function handleSave() {
    setSaving(true);
    setSaveNote(null);
    setSaveError(null);
    try {
      const text = emitWorkflow(currentWorkflow());
      const result = await validateWorkflow(text);

      if (!result.valid) {
        setErrorCodes(errorCodesByAction(result.errors));
        const count = (result.errors ?? []).length;
        setSaveNote(`Invalid: ${String(count)} problem(s). Codes shown on affected nodes.`);
        return;
      }

      // A clean validation clears any node markers left from a prior attempt.
      setErrorCodes({});
      const res = await saveWorkflow(meta.name, text);
      setSaveNote(`Saved ${res.workflowName} (hash ${res.hash}${res.created ? ', created' : ''}).`);
    } catch (err) {
      setSaveError(err);
    } finally {
      setSaving(false);
    }
  }

  function handleRun() {
    void start(meta.name, parseInputs(inputsText));
  }

  function handleTest() {
    if (!selectedId) {
      return;
    }
    const test = buildTestWorkflow(currentWorkflow(), selectedId);
    void start(meta.name, parseInputs(inputsText), { toml: emitWorkflow(test) });
  }

  return (
    <section className="space-y-3">
      <h3 className="text-sm font-semibold">Save and run</h3>

      <label className="space-y-1 text-sm">
        <span className="block font-medium">Inputs (key=value per line)</span>
        <textarea
          value={inputsText}
          onChange={(event) => setInputsText(event.target.value)}
          rows={2}
          className="w-72 rounded border border-gray-300 px-2 py-1 font-mono text-xs"
        />
      </label>

      <div className="flex flex-wrap gap-2">
        <button
          type="button"
          onClick={() => void handleSave()}
          disabled={saving}
          className="rounded bg-gray-900 px-3 py-1 text-sm text-white disabled:opacity-50"
        >
          {saving ? 'Saving...' : 'Save'}
        </button>
        <button
          type="button"
          onClick={handleRun}
          disabled={state.running}
          className="rounded bg-gray-900 px-3 py-1 text-sm text-white disabled:opacity-50"
        >
          Run
        </button>
        <button
          type="button"
          onClick={handleTest}
          disabled={state.running || !selectedId}
          className="rounded border border-gray-300 px-3 py-1 text-sm disabled:opacity-50"
        >
          Test selected action
        </button>
      </div>

      {saveNote ? <p className="text-sm text-gray-600">{saveNote}</p> : null}
      {saveError ? <ErrorBanner error={saveError} title="Save failed" /> : null}

      <LiveRunPanel state={state} cancel={cancel} />
    </section>
  );
}

/**
 * defaultInputsText prefills the inputs field from the declared defaults so a
 * run starts from the workflow's own suggested values. Inputs without a
 * declared default are left out, matching the free-form key=value shape the
 * run stream reads.
 */
function defaultInputsText(inputs: Workflow['inputs']): string {
  const lines: string[] = [];
  for (const [name, decl] of Object.entries(inputs)) {
    if (decl.hasDefault && decl.default !== undefined) {
      lines.push(`${name}=${scalarText(decl.default)}`);
    }
  }
  return lines.join('\n');
}

/**
 * scalarText renders a declared default as a key=value string. Inputs are
 * scalars or a string list; a list joins on commas and anything object-shaped
 * (which the schema never produces for a default) yields empty rather than
 * "[object Object]".
 */
function scalarText(value: JsonValue): string {
  if (typeof value === 'string') {
    return value;
  }
  if (typeof value === 'number' || typeof value === 'boolean') {
    return String(value);
  }
  if (Array.isArray(value)) {
    return value.map(scalarText).join(',');
  }
  return '';
}
