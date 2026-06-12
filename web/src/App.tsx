import { Suspense, lazy, useState } from 'react';
import { Navigate, Route, Routes } from 'react-router-dom';

import { AppShell } from '@/components/AppShell';
import { Spinner } from '@/components/Spinner';
import { useAuth } from '@/auth/context';
import { ChatPage } from '@/pages/ChatPage';
import { GovernancePage } from '@/pages/GovernancePage';
import { LoginPage } from '@/pages/LoginPage';
import { RunDetailPage } from '@/pages/RunDetailPage';
import { RunMonitorPage } from '@/pages/RunMonitorPage';
import { RunnersPage } from '@/pages/RunnersPage';
import { RunsListPage } from '@/pages/RunsListPage';
import { SuggestionsPage } from '@/pages/SuggestionsPage';

// The builder pulls in reactflow, the heaviest dependency. Lazy-loading it
// keeps that chunk out of the initial bundle for users who land on runs or
// suggestions first.
const BuilderPage = lazy(() =>
  import('@/pages/BuilderPage').then((mod) => ({ default: mod.BuilderPage })),
);

/**
 * App decides between the login screen and the gated shell. The gate is soft:
 * Health needs no token, so a user on a no-auth server can skip sign-in and
 * still use every read. If a token-requiring call returns 401, the auth
 * context flips `authed` off and the login screen returns. `skipped` lets a
 * no-auth user dismiss login without a token.
 */
export function App() {
  const { authed } = useAuth();
  const [skipped, setSkipped] = useState(false);

  if (!authed && !skipped) {
    return <LoginPage onSkip={() => setSkipped(true)} />;
  }

  return (
    <Routes>
      <Route element={<AppShell />}>
        <Route index element={<Navigate to="/builder" replace />} />
        <Route
          path="builder"
          element={
            <Suspense fallback={<Spinner label="Loading builder" />}>
              <BuilderPage />
            </Suspense>
          }
        />
        <Route path="chat" element={<ChatPage />} />
        <Route path="run" element={<RunMonitorPage />} />
        <Route path="runs" element={<RunsListPage />} />
        <Route path="runs/:runId" element={<RunDetailPage />} />
        <Route path="runners" element={<RunnersPage />} />
        <Route path="suggestions" element={<SuggestionsPage />} />
        <Route path="governance" element={<GovernancePage />} />
        <Route path="*" element={<Navigate to="/builder" replace />} />
      </Route>
    </Routes>
  );
}
