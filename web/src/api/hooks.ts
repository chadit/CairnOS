// TanStack Query hooks over the endpoint functions. Components read server
// state through these so caching, loading, and error states are consistent.

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { createSession, listMessages, listSessions } from './chat';
import {
  approveSuggestion,
  getRun,
  health,
  listRunners,
  listRuns,
  listSuggestions,
  rejectSuggestion,
} from './endpoints';

export const queryKeys = {
  health: ['health'] as const,
  runners: ['runners'] as const,
  runs: (workflow?: string, status?: string) => ['runs', workflow ?? '', status ?? ''] as const,
  run: (runId: string) => ['run', runId] as const,
  suggestions: (status?: string) => ['suggestions', status ?? ''] as const,
  chatSessions: ['chat', 'sessions'] as const,
  chatMessages: (sessionId: string) => ['chat', 'messages', sessionId] as const,
};

export function useHealth() {
  return useQuery({
    queryKey: queryKeys.health,
    queryFn: health,
    retry: false,
    staleTime: 10_000,
  });
}

export function useRunners() {
  return useQuery({ queryKey: queryKeys.runners, queryFn: listRunners });
}

export function useRuns(workflow?: string, status?: string) {
  return useQuery({
    queryKey: queryKeys.runs(workflow, status),
    queryFn: () => listRuns({ workflow, status }),
    refetchInterval: 5_000,
  });
}

export function useRun(runId: string | undefined) {
  return useQuery({
    queryKey: queryKeys.run(runId ?? ''),
    queryFn: () => getRun(runId!),
    enabled: Boolean(runId),
  });
}

export function useSuggestions(status?: string) {
  return useQuery({
    queryKey: queryKeys.suggestions(status),
    queryFn: () => listSuggestions({ status }),
  });
}

export function useApproveSuggestion() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => approveSuggestion(id),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['suggestions'] });
    },
  });
}

export function useRejectSuggestion() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => rejectSuggestion(id),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['suggestions'] });
    },
  });
}

export function useChatSessions() {
  return useQuery({ queryKey: queryKeys.chatSessions, queryFn: () => listSessions() });
}

/** useChatMessages loads one session's stored transcript. Disabled until a
 * session is selected so it does not fire on the empty-state view. */
export function useChatMessages(sessionId: string | null) {
  return useQuery({
    queryKey: queryKeys.chatMessages(sessionId ?? ''),
    queryFn: () => listMessages(sessionId!),
    enabled: Boolean(sessionId),
  });
}

export function useCreateChatSession() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (personality: string) => createSession(personality),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.chatSessions });
    },
  });
}
