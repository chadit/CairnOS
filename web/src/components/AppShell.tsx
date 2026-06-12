import { NavLink, Outlet } from 'react-router-dom';

import { useHealth } from '@/api/hooks';
import { useAuth } from '@/auth/context';

const NAV = [
  { to: '/builder', label: 'Builder' },
  { to: '/chat', label: 'Chat' },
  { to: '/run', label: 'Run monitor' },
  { to: '/runs', label: 'Runs' },
  { to: '/runners', label: 'Runners' },
  { to: '/suggestions', label: 'Suggestions' },
  { to: '/governance', label: 'Governance' },
];

/** AppShell is the gated frame: a header with health, nav, and the routed page. */
export function AppShell() {
  const { authed, logout } = useAuth();
  const healthQuery = useHealth();

  const healthLabel =
    healthQuery.data?.status === 'ok'
      ? 'gateway ok'
      : healthQuery.isError
        ? 'gateway unreachable'
        : 'checking gateway';

  return (
    <div className="min-h-screen bg-gray-50 text-gray-900">
      <header className="border-b border-gray-200 bg-white">
        <div className="mx-auto flex max-w-6xl flex-wrap items-center gap-4 px-4 py-3">
          <span className="text-lg font-semibold">CairnOS</span>
          <nav className="flex flex-wrap gap-1" aria-label="Main">
            {NAV.map((item) => (
              <NavLink
                key={item.to}
                to={item.to}
                className={({ isActive }) =>
                  `rounded px-3 py-1 text-sm ${
                    isActive ? 'bg-gray-900 text-white' : 'text-gray-600 hover:bg-gray-100'
                  }`
                }
              >
                {item.label}
              </NavLink>
            ))}
          </nav>
          <div className="ml-auto flex items-center gap-3 text-sm">
            <span
              className="text-gray-500"
              aria-live="polite"
              data-status={healthQuery.data?.status === 'ok' ? 'ok' : 'down'}
            >
              {healthLabel}
            </span>
            {authed ? (
              <button type="button" onClick={logout} className="text-gray-600 underline">
                Sign out
              </button>
            ) : null}
          </div>
        </div>
      </header>

      <main className="mx-auto max-w-6xl px-4 py-6">
        <Outlet />
      </main>
    </div>
  );
}
