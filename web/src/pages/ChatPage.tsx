import { useEffect, useState } from 'react';

import { useChatMessages, useChatSessions, useCreateChatSession } from '@/api/hooks';
import { ErrorBanner } from '@/components/ErrorBanner';
import { Composer } from '@/chat/Composer';
import { ConfirmationCard } from '@/chat/ConfirmationCard';
import { SessionSidebar } from '@/chat/SessionSidebar';
import { Transcript } from '@/chat/Transcript';
import { useChatStream } from '@/chat/useChatStream';

/**
 * ChatPage is the conversational view. The sidebar lists sessions and starts
 * new ones with a personality; the main column shows the transcript and a
 * composer. Stored messages seed the transcript and live SendMessage events
 * fold on top of them.
 *
 * The confirmation gate is the point of the page. When the stream yields a
 * CONFIRMATION_REQUIRED proposal, useChatStream parks it and the page renders a
 * ConfirmationCard. Nothing runs until the user clicks Approve or Deny, which
 * calls ConfirmAction and resumes the stream. A stray proposal cannot auto-run.
 */
export function ChatPage() {
  const sessionsQuery = useChatSessions();
  const createSession = useCreateChatSession();
  const [activeId, setActiveId] = useState<string | null>(null);

  const messagesQuery = useChatMessages(activeId);
  const { state, seed, reset, send, resolve } = useChatStream();

  // When the selected session changes, reset live state and seed from its
  // stored transcript once that query resolves.
  useEffect(() => {
    if (!activeId) {
      reset();
      return;
    }
    if (messagesQuery.data) {
      seed(messagesQuery.data.messages ?? []);
    }
  }, [activeId, messagesQuery.data, seed, reset]);

  function handleCreate(personality: string) {
    createSession.mutate(personality, {
      onSuccess: (res) => setActiveId(res.session.id),
    });
  }

  function handleSend(text: string) {
    if (activeId) {
      void send(activeId, text);
    }
  }

  function handleResolve(approve: boolean) {
    if (activeId) {
      void resolve(activeId, approve);
    }
  }

  const sessions = sessionsQuery.data?.sessions ?? [];

  return (
    <div className="flex gap-6">
      <SessionSidebar
        sessions={sessions}
        activeId={activeId}
        loading={sessionsQuery.isLoading}
        error={sessionsQuery.isError ? sessionsQuery.error : null}
        creating={createSession.isPending}
        onSelect={setActiveId}
        onCreate={handleCreate}
      />

      <section className="flex min-h-[28rem] flex-1 flex-col gap-4">
        <h1 className="text-xl font-semibold">Chat</h1>

        {createSession.isError ? (
          <ErrorBanner error={createSession.error} title="Could not start session" />
        ) : null}

        {!activeId ? (
          <p className="text-sm text-gray-500">
            Start a session or pick one from the sidebar to begin.
          </p>
        ) : (
          <>
            {messagesQuery.isError ? (
              <ErrorBanner error={messagesQuery.error} title="Could not load messages" />
            ) : null}

            <div className="flex-1 overflow-y-auto rounded border border-gray-200 bg-gray-50 p-4">
              <Transcript rows={state.rows} streaming={state.streaming} />
            </div>

            {state.pending ? (
              <ConfirmationCard
                proposal={state.pending.proposal}
                busy={state.streaming}
                onApprove={() => handleResolve(true)}
                onDeny={() => handleResolve(false)}
              />
            ) : null}

            {state.error ? (
              <ErrorBanner
                error={Object.assign(new Error(state.error.message), { code: state.error.code })}
                title="Chat error"
              />
            ) : null}

            <Composer disabled={state.streaming} onSend={handleSend} />
          </>
        )}
      </section>
    </div>
  );
}
