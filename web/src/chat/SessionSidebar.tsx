import { type FormEvent, useState } from 'react';

import type { ChatSession } from '@/api/types';
import { ErrorBanner } from '@/components/ErrorBanner';
import { Spinner } from '@/components/Spinner';

interface SessionSidebarProps {
  sessions: ChatSession[];
  activeId: string | null;
  loading: boolean;
  error: unknown;
  creating: boolean;
  onSelect: (sessionId: string) => void;
  onCreate: (personality: string) => void;
}

// Built-in personalities the gateway ships. The empty value maps to the server
// default; we send no personality field for it (see createSession).
const PERSONALITIES = ['', 'concise', 'thorough', 'playful'] as const;

function personalityLabel(value: string): string {
  return value === '' ? 'Default' : value;
}

/**
 * SessionSidebar lists conversations and starts a new one with a chosen
 * personality. Picking a session calls onSelect; starting one calls onCreate.
 * It does not own session data; the page does and passes it down.
 */
export function SessionSidebar({
  sessions,
  activeId,
  loading,
  error,
  creating,
  onSelect,
  onCreate,
}: SessionSidebarProps) {
  const [personality, setPersonality] = useState('');

  function handleCreate(event: FormEvent) {
    event.preventDefault();
    onCreate(personality);
  }

  return (
    <aside className="w-60 shrink-0 space-y-4" aria-label="Sessions">
      <form onSubmit={handleCreate} className="space-y-2">
        <label className="block space-y-1 text-sm">
          <span className="block font-medium">Personality</span>
          <select
            value={personality}
            onChange={(event) => setPersonality(event.target.value)}
            className="w-full rounded border border-gray-300 px-2 py-1 text-sm"
          >
            {PERSONALITIES.map((value) => (
              <option key={value || 'default'} value={value}>
                {personalityLabel(value)}
              </option>
            ))}
          </select>
        </label>
        <button
          type="submit"
          disabled={creating}
          className="w-full rounded bg-gray-900 px-3 py-1 text-sm text-white disabled:opacity-50"
        >
          {creating ? 'Starting...' : 'New session'}
        </button>
      </form>

      <div className="space-y-1">
        <h2 className="text-xs font-semibold uppercase tracking-wide text-gray-500">Sessions</h2>
        {loading ? (
          <Spinner label="Loading sessions" />
        ) : error ? (
          <ErrorBanner error={error} title="Could not load sessions" />
        ) : sessions.length === 0 ? (
          <p className="text-sm text-gray-500">No sessions yet.</p>
        ) : (
          <ul className="space-y-1">
            {sessions.map((session) => {
              const active = session.id === activeId;
              return (
                <li key={session.id}>
                  <button
                    type="button"
                    onClick={() => onSelect(session.id)}
                    aria-current={active ? 'true' : undefined}
                    className={`w-full rounded px-2 py-1 text-left text-sm ${
                      active ? 'bg-gray-900 text-white' : 'text-gray-700 hover:bg-gray-100'
                    }`}
                  >
                    <span className="block truncate font-mono text-xs">{session.id}</span>
                    {session.personality ? (
                      <span
                        className={`block text-xs ${active ? 'text-gray-300' : 'text-gray-500'}`}
                      >
                        {session.personality}
                      </span>
                    ) : null}
                  </button>
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </aside>
  );
}
