import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { BrowserRouter } from 'react-router-dom';

import { App } from '@/App';
import { AuthProvider } from '@/auth/AuthContext';
import { loadToken } from '@/api/token';

import './index.css';

// Seed the in-memory token from storage before the first render so a refresh
// keeps the session.
loadToken();

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // A failed read should surface its error promptly rather than retry into
      // a spinner; the error envelope is the useful signal.
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
});

const rootElement = document.getElementById('root');
if (!rootElement) {
  throw new Error('root element missing from index.html');
}

createRoot(rootElement).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <AuthProvider>
          <App />
        </AuthProvider>
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
);
