import { useEffect } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Navigate, Outlet, Route, Routes, useNavigate } from 'react-router-dom'
import { AppShell } from './components/AppShell'
import { Alert, Button, Spinner } from './components/ui'
import { UNAUTHORIZED_EVENT, type AuthStatus } from './lib/api'
import { queryKeys, useAuthStatus, useMe } from './lib/hooks'
import { ThemeProvider } from './lib/ThemeProvider'
import { LibraryPage } from './pages/Library'
import { LoginPage } from './pages/Login'
import { LogsPage } from './pages/Logs'
import { QueuePage } from './pages/Queue'
import { SettingsPage } from './pages/Settings'
import { SetupPage } from './pages/Setup'
import { SyncPage } from './pages/Sync'
import { SystemPage } from './pages/System'
import { TokensPage } from './pages/Tokens'
import { VaultPage } from './pages/Vault'

/**
 * Route gate: /api/auth/status decides whether the appliance needs first-run
 * setup, a login, or is ready for the authenticated shell.
 */
function AuthGate() {
  const status = useAuthStatus()

  if (status.isPending) {
    return (
      <div className="grid min-h-full place-items-center bg-ink-950">
        <Spinner label="Loading Dubarr…" />
      </div>
    )
  }

  if (status.isError) {
    return (
      <div className="grid min-h-full place-items-center bg-ink-950 p-6">
        <div className="w-full max-w-md space-y-3">
          <Alert>Could not reach Dubarr: {status.error.message}</Alert>
          <Button onClick={() => void status.refetch()}>Retry</Button>
        </div>
      </div>
    )
  }

  if (!status.data.initialized) return <Navigate to="/setup" replace />
  if (!status.data.authenticated) return <Navigate to="/login" replace />
  return <Outlet />
}

/**
 * Admin-only route: /api/auth/me decides. Tokens are an admin capability, so a
 * non-admin who guesses the URL is sent back to the library instead.
 */
function AdminRoute() {
  const me = useMe()

  if (me.isPending) {
    return (
      <div className="py-10">
        <Spinner label="Checking permissions…" />
      </div>
    )
  }

  if (me.isError) {
    return <Alert>Could not verify admin access: {me.error.message}</Alert>
  }

  if (!me.data.is_admin) return <Navigate to="/library" replace />
  return <Outlet />
}

function App() {
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  // Any 401 anywhere in the app (wizard, login, shell) drops the session and
  // returns to sign-in instead of dead-ending on a stale route.
  useEffect(() => {
    const handler = () => {
      queryClient.setQueryData<AuthStatus>(queryKeys.authStatus, (current) =>
        current ? { ...current, authenticated: false } : current,
      )
      navigate('/login', { replace: true })
    }
    window.addEventListener(UNAUTHORIZED_EVENT, handler)
    return () => window.removeEventListener(UNAUTHORIZED_EVENT, handler)
  }, [navigate, queryClient])

  return (
    <ThemeProvider>
      <Routes>
        <Route path="/setup" element={<SetupPage />} />
        <Route path="/login" element={<LoginPage />} />
        <Route element={<AuthGate />}>
          <Route element={<AppShell />}>
            <Route index element={<Navigate to="/library" replace />} />
            <Route path="/library" element={<LibraryPage />} />
            <Route
              path="/queue"
              element={<QueuePage />}
            />
            <Route path="/vault" element={<VaultPage />} />
            <Route path="/sync" element={<SyncPage />} />
            <Route path="/settings" element={<SettingsPage />} />
            <Route path="/system" element={<SystemPage />} />
            <Route element={<AdminRoute />}>
              <Route path="/tokens" element={<TokensPage />} />
              <Route path="/logs" element={<LogsPage />} />
            </Route>
            <Route path="*" element={<Navigate to="/library" replace />} />
          </Route>
        </Route>
      </Routes>
    </ThemeProvider>
  )
}

export default App
