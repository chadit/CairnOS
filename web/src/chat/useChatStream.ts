import { useCallback, useRef, useState } from 'react';

import type { ApiError, ChatEvent, ChatMessage } from '@/api/types';
import { confirmAction, sendMessage } from '@/api/chat';
import { isCairnApiError } from '@/api/client';
import type { PendingProposal, TranscriptRow } from './types';

interface ChatStreamState {
  /** rows is the ordered transcript: stored history plus live turn output. */
  rows: TranscriptRow[];
  /** streaming is true while a turn's events are arriving. */
  streaming: boolean;
  /** pending holds a workflow proposal awaiting the user's decision. While it
   * is set, nothing runs; the gate is surfaced and the user must answer. */
  pending: PendingProposal | null;
  error: ApiError | null;
}

const INITIAL: ChatStreamState = {
  rows: [],
  streaming: false,
  pending: null,
  error: null,
};

/** rowId makes a stable-enough key for a row. Stored messages key on their
 * server seq; live rows key on a monotonic counter so React keeps bubbles
 * distinct as they stream in. */
function storedRowId(message: ChatMessage, index: number): string {
  return `stored-${message.seq ?? index}`;
}

/**
 * useChatStream drives one session's transcript. It seeds from stored messages,
 * then folds each ChatEvent of a turn into the rows: ASSISTANT_CHUNK appends to
 * the current assistant bubble, TOOL_CALL adds a "used <tool>" note, and
 * RUN_STARTED/RUN_FINISHED add or update a run row.
 *
 * The confirmation gate is the load-bearing part. A CONFIRMATION_REQUIRED event
 * does not run anything; it parks the proposal in `pending` and the stream
 * pauses there. Only approve()/deny() resume by calling ConfirmAction. A stray
 * proposal can never auto-run because the fold has no path from a proposal to a
 * ConfirmAction call.
 */
export function useChatStream() {
  const [state, setState] = useState<ChatStreamState>(INITIAL);
  const abortRef = useRef<AbortController | null>(null);
  // liveSeq numbers live rows so React keys stay unique within a session.
  const liveSeqRef = useRef(0);

  const nextLiveId = useCallback((prefix: string): string => {
    liveSeqRef.current += 1;
    return `live-${prefix}-${liveSeqRef.current}`;
  }, []);

  /** seed replaces the transcript with stored history when a session loads. */
  const seed = useCallback((messages: ChatMessage[]): void => {
    liveSeqRef.current = 0;
    const rows = messages.map((message, index): TranscriptRow => {
      const id = storedRowId(message, index);
      if (message.role === 'tool') {
        return { kind: 'tool', id, tool: message.content };
      }
      if (message.role === 'assistant') {
        return { kind: 'assistant', id, text: message.content };
      }
      return { kind: 'user', id, text: message.content };
    });
    setState({ rows, streaming: false, pending: null, error: null });
  }, []);

  const reset = useCallback((): void => {
    abortRef.current?.abort();
    liveSeqRef.current = 0;
    setState(INITIAL);
  }, []);

  /** consume folds a stream of ChatEvents into the transcript until the stream
   * ends or a proposal parks the turn. Shared by send and approve/deny. */
  const consume = useCallback(
    async (
      events: AsyncGenerator<ChatEvent, void, void>,
      controller: AbortController,
    ): Promise<void> => {
      setState((prev) => ({ ...prev, streaming: true, error: null }));
      // A fresh assistant bubble id per turn so chunks of one reply coalesce
      // into a single bubble rather than spawning one bubble per chunk.
      let assistantId: string | null = null;

      try {
        for await (const event of events) {
          setState((prev) => {
            const folded = foldEvent(prev, event, assistantId, nextLiveId);
            assistantId = folded.assistantId;
            return folded.state;
          });
        }
        setState((prev) => ({ ...prev, streaming: false }));
      } catch (err) {
        if (controller.signal.aborted) {
          setState((prev) => ({ ...prev, streaming: false }));
          return;
        }
        const apiError: ApiError = isCairnApiError(err)
          ? { code: err.code, message: err.message }
          : { code: 'chat.stream_error', message: String(err) };
        setState((prev) => ({ ...prev, streaming: false, error: apiError }));
      }
    },
    [nextLiveId],
  );

  /** send posts a user turn, appends the user bubble, and streams the reply. */
  const send = useCallback(
    async (sessionId: string, content: string): Promise<void> => {
      const text = content.trim();
      if (text.length === 0) {
        return;
      }
      abortRef.current?.abort();
      const controller = new AbortController();
      abortRef.current = controller;

      setState((prev) => ({
        ...prev,
        rows: [...prev.rows, { kind: 'user', id: nextLiveId('user'), text }],
        pending: null,
      }));

      await consume(sendMessage(sessionId, text, controller.signal), controller);
    },
    [consume, nextLiveId],
  );

  /**
   * resolve answers a pending proposal. It calls ConfirmAction with the user's
   * decision and folds the follow-on stream. This is the ONLY path that runs a
   * proposed workflow, and it runs only because the user clicked.
   */
  const resolve = useCallback(
    async (sessionId: string, approve: boolean): Promise<void> => {
      abortRef.current?.abort();
      const controller = new AbortController();
      abortRef.current = controller;

      // Clearing pending first removes the gate from view; the decision is made.
      setState((prev) => ({ ...prev, pending: null }));

      await consume(confirmAction(sessionId, approve, controller.signal), controller);
    },
    [consume],
  );

  const cancel = useCallback((): void => {
    abortRef.current?.abort();
  }, []);

  return { state, seed, reset, send, resolve, cancel };
}

interface FoldResult {
  state: ChatStreamState;
  assistantId: string | null;
}

/** foldEvent applies one ChatEvent to the transcript. assistantId carries the
 * id of the bubble that chunks of the current reply append to. */
function foldEvent(
  prev: ChatStreamState,
  event: ChatEvent,
  assistantId: string | null,
  nextLiveId: (prefix: string) => string,
): FoldResult {
  switch (event.type) {
    case 'CHAT_EVENT_TYPE_ASSISTANT_CHUNK': {
      const chunk = event.text ?? '';
      if (chunk.length === 0) {
        return { state: prev, assistantId };
      }
      if (assistantId === null) {
        const id = nextLiveId('assistant');
        return {
          state: { ...prev, rows: [...prev.rows, { kind: 'assistant', id, text: chunk }] },
          assistantId: id,
        };
      }
      const rows = appendToAssistant(prev.rows, assistantId, chunk);
      return { state: { ...prev, rows }, assistantId };
    }

    case 'CHAT_EVENT_TYPE_TOOL_CALL': {
      const tool = event.tool ?? 'unknown tool';
      const rows = [...prev.rows, { kind: 'tool' as const, id: nextLiveId('tool'), tool }];
      return { state: { ...prev, rows }, assistantId };
    }

    case 'CHAT_EVENT_TYPE_CONFIRMATION_REQUIRED': {
      // Park the proposal. No run is triggered here; the user decides next.
      if (!event.proposal) {
        return { state: prev, assistantId };
      }
      return {
        state: { ...prev, pending: { proposal: event.proposal } },
        assistantId,
      };
    }

    case 'CHAT_EVENT_TYPE_RUN_STARTED': {
      const runId = event.runId ?? '';
      const rows = [
        ...prev.rows,
        { kind: 'run' as const, id: `run-${runId || nextLiveId('run')}`, runId, status: 'running' },
      ];
      return { state: { ...prev, rows }, assistantId };
    }

    case 'CHAT_EVENT_TYPE_RUN_FINISHED': {
      const runId = event.runId ?? '';
      const status = event.runStatus ?? 'finished';
      const rows = updateRun(prev.rows, runId, status, nextLiveId);
      return { state: { ...prev, rows }, assistantId };
    }

    case 'CHAT_EVENT_TYPE_TURN_DONE':
    case 'CHAT_EVENT_TYPE_UNSPECIFIED':
    default:
      // TURN_DONE carries the final reply text, already streamed via chunks;
      // nothing to append. The unspecified case is ignored defensively.
      return { state: prev, assistantId };
  }
}

/** appendToAssistant grows the named assistant bubble with the next chunk. */
function appendToAssistant(rows: TranscriptRow[], id: string, chunk: string): TranscriptRow[] {
  return rows.map((row) =>
    row.kind === 'assistant' && row.id === id ? { ...row, text: row.text + chunk } : row,
  );
}

/** updateRun marks an existing run row finished, or adds one if RUN_STARTED was
 * missed, so the status is always visible. */
function updateRun(
  rows: TranscriptRow[],
  runId: string,
  status: string,
  nextLiveId: (prefix: string) => string,
): TranscriptRow[] {
  const index = rows.findIndex((row) => row.kind === 'run' && row.runId === runId);
  if (index === -1) {
    return [...rows, { kind: 'run', id: `run-${runId || nextLiveId('run')}`, runId, status }];
  }
  const copy = [...rows];
  const existing = copy[index];
  if (existing?.kind === 'run') {
    copy[index] = { ...existing, status };
  }
  return copy;
}
