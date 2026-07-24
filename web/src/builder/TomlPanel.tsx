import { useState } from 'react';

import { validateWorkflow } from '@/api/endpoints';
import { ErrorBanner } from '@/components/ErrorBanner';
import { emitWorkflow } from '@/workflow/emit';
import { WorkflowParseError, parseWorkflow } from '@/workflow/parse';
import { useBuilderStore } from './store';
import { errorCodesByAction } from './validationErrors';

/**
 * TomlPanel is the export/import bridge. Export renders the current canvas to
 * TOML and validates it through the gateway, mapping any errors back onto the
 * nodes by action name. Import parses pasted TOML into the graph. The round-
 * trip is semantic: comments and formatting are not preserved.
 */
export function TomlPanel() {
  const currentWorkflow = useBuilderStore((state) => state.currentWorkflow);
  const loadWorkflow = useBuilderStore((state) => state.loadWorkflow);
  const setErrorCodes = useBuilderStore((state) => state.setErrorCodes);

  const [toml, setToml] = useState('');
  const [importError, setImportError] = useState<unknown>(null);
  const [validating, setValidating] = useState(false);
  const [validationNote, setValidationNote] = useState<string | null>(null);

  function handleExport() {
    setToml(emitWorkflow(currentWorkflow()));
    setValidationNote(null);
  }

  function handleImport() {
    setImportError(null);
    try {
      const flow = parseWorkflow(toml);
      loadWorkflow(flow);
      setValidationNote('Imported. The canvas now reflects the pasted TOML.');
    } catch (err) {
      if (err instanceof WorkflowParseError) {
        setImportError(err);
        return;
      }
      setImportError(err);
    }
  }

  async function handleValidate() {
    setValidating(true);
    setValidationNote(null);
    try {
      const text = toml || emitWorkflow(currentWorkflow());
      setToml(text);
      const result = await validateWorkflow(text);

      setErrorCodes(errorCodesByAction(result.errors));

      setValidationNote(
        result.valid
          ? 'Valid. The gateway accepts this workflow.'
          : `Invalid: ${(result.errors ?? []).length} problem(s). Codes shown on affected nodes.`,
      );
    } catch (err) {
      setImportError(err);
    } finally {
      setValidating(false);
    }
  }

  return (
    <section className="space-y-2">
      <div className="flex flex-wrap gap-2">
        <button
          type="button"
          onClick={handleExport}
          className="rounded bg-gray-200 px-3 py-1 text-sm"
        >
          Export to TOML
        </button>
        <button
          type="button"
          onClick={handleImport}
          className="rounded bg-gray-200 px-3 py-1 text-sm"
        >
          Import from TOML
        </button>
        <button
          type="button"
          onClick={() => void handleValidate()}
          disabled={validating}
          className="rounded bg-gray-900 px-3 py-1 text-sm text-white disabled:opacity-50"
        >
          {validating ? 'Validating...' : 'Validate'}
        </button>
      </div>

      <label className="sr-only" htmlFor="toml-text">
        Workflow TOML
      </label>
      <textarea
        id="toml-text"
        value={toml}
        onChange={(event) => setToml(event.target.value)}
        rows={16}
        spellCheck={false}
        placeholder="Export the canvas here, or paste a workflow to import."
        className="w-full rounded border border-gray-300 p-2 font-mono text-xs"
      />

      {validationNote ? <p className="text-sm text-gray-600">{validationNote}</p> : null}
      {importError ? <ErrorBanner error={importError} title="TOML problem" /> : null}
    </section>
  );
}
