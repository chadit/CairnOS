import { useCallback, useRef, useState } from 'react';

import type { ApiError, RunEvent } from '@/api/types';
import { isCairnApiError } from '@/api/client';
import { streamRunEvents } from '@/api/stream';

/** ActionLive is one action's running state, accumulated from the stream. */
export interface ActionLive {
  name: string;
  status: string;
  attempt: number;
}

export interface RunStreamState {
  running: boolean;
  runId: string | null;
  finalStatus: string | null;
  actions: ActionLive[];
  events: RunEvent[];
  error: ApiError | null;
}

const INITIAL: RunStreamState = {
  running: false,
  runId: null,
  finalStatus: null,
  actions: [],
  events: [],
  error: null,
};

/**
 * useRunStream drives a single RunWorkflow stream. start() opens the stream,
 * folds each event into per-action live status, and records the terminal run
 * status. cancel() aborts the fetch. Action order follows first appearance in
 * the stream so the table reads top to bottom in run order.
 */
export function useRunStream() {
  const [state, setState] = useState<RunStreamState>(INITIAL);
  const abortRef = useRef<AbortController | null>(null);

  const cancel = useCallback(() => {
    abortRef.current?.abort();
  }, []);

  const start = useCallback(async (workflowName: string, inputs: Record<string, string>) => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;

    setState({ ...INITIAL, running: true });

    try {
      for await (const event of streamRunEvents(workflowName, inputs, controller.signal)) {
        setState((prev) => fold(prev, event));
      }
      setState((prev) => ({ ...prev, running: false }));
    } catch (err) {
      // An abort is a deliberate cancel, not an error to surface.
      if (controller.signal.aborted) {
        setState((prev) => ({ ...prev, running: false }));
        return;
      }
      const apiError: ApiError = isCairnApiError(err)
        ? { code: err.code, message: err.message }
        : { code: 'stream.error', message: String(err) };
      setState((prev) => ({ ...prev, running: false, error: apiError }));
    }
  }, []);

  return { state, start, cancel };
}

function fold(prev: RunStreamState, event: RunEvent): RunStreamState {
  const events = [...prev.events, event];
  const next: RunStreamState = { ...prev, events, runId: event.runId || prev.runId };

  if (event.type === 'RUN_EVENT_TYPE_ACTION_TRANSITION' && event.action) {
    next.actions = upsertAction(prev.actions, event.action);
  }

  if (event.type === 'RUN_EVENT_TYPE_RUN_FINISHED') {
    next.running = false;
    next.finalStatus = event.run?.status ?? null;
    if (event.error) {
      next.error = event.error;
    }
  }

  return next;
}

function upsertAction(
  actions: ActionLive[],
  state: { name: string; status: string; attempt?: number },
): ActionLive[] {
  const index = actions.findIndex((item) => item.name === state.name);
  const live: ActionLive = { name: state.name, status: state.status, attempt: state.attempt ?? 0 };
  if (index === -1) {
    return [...actions, live];
  }
  const copy = [...actions];
  copy[index] = live;
  return copy;
}
