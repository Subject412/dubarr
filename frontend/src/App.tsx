import { Navigate, Outlet, Route, Routes } from 'react-router-dom'
import { AppShell } from './components/AppShell'
import { Alert, Button, Spinner } from './components/ui'
import { useAuthStatus } from './lib/hooks'
import { LoginPage } from './pages/Login'
import { PlaceholderPage } from './pages/Placeholder'
import { SettingsPage } from './pages/Settings'
import { SetupPage } from './pages/Setup'
import { SystemPage } from './pages/System'

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

function App() {
  return (
    <Routes>
      <Route path="/setup" element={<SetupPage />} />
      <Route path="/login" element={<LoginPage />} />
      <Route element={<AuthGate />}>
        <Route element={<AppShell />}>
          <Route index element={<Navigate to="/library" replace />} />
          <Route
            path="/library"
            element={
              <PlaceholderPage
                title="Library"
                milestone="M2"
                description="Browse titles, inspect tracks and start a sync job."
              />
            }
          />
          <Route
            path="/sync"
            element={
              <PlaceholderPage
                title="Sync Studio"
                milestone="M5"
                description="Checkpoint previews, offset tuning and mux review."
              />
            }
          />
          <Route path="/settings" element={<SettingsPage />} />
          <Route path="/system" element={<SystemPage />} />
          <Route path="*" element={<Navigate to="/library" replace />} />
        </Route>
      </Route>
    </Routes>
  )
}

export default App
