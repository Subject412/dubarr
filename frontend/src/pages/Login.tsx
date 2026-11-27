import { useState, type FormEvent } from 'react'
import { Navigate, useNavigate } from 'react-router-dom'
import { useAuthStatus, useLogin } from '../lib/hooks'
import { Alert, Button, Field, Input } from '../components/ui'

export function LoginPage() {
  const navigate = useNavigate()
  const login = useLogin()
  const status = useAuthStatus()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')

  if (status.data?.authenticated) return <Navigate to="/library" replace />

  const submit = (event: FormEvent) => {
    event.preventDefault()
    login.mutate(
      { username: username.trim(), password },
      { onSuccess: () => navigate('/library', { replace: true }) },
    )
  }

  return (
    <div className="grid min-h-full place-items-center bg-ink-950 px-4 py-16">
      <div className="w-full max-w-sm">
        <div className="mb-8 flex items-center gap-3">
          <img
            src="/icon-512.png"
            alt=""
            aria-hidden="true"
            className="h-12 w-12 object-contain"
          />
          <div>
            <h1 className="text-xl font-semibold tracking-tight text-zinc-100">Dubarr</h1>
            <p className="text-xs uppercase tracking-widest text-zinc-500">dubbing sync appliance</p>
          </div>
        </div>

        <form
          onSubmit={submit}
          className="space-y-4 rounded-xl border border-ink-800 bg-ink-900 p-6 shadow-xl"
        >
          <div>
            <h2 className="text-base font-semibold text-zinc-100">Sign in</h2>
            <p className="text-sm text-zinc-400">Use the administrator account for this appliance.</p>
          </div>

          {login.error && <Alert>{login.error.message}</Alert>}

          <Field label="Username" htmlFor="login-username">
            <Input
              id="login-username"
              name="username"
              autoComplete="username"
              autoFocus
              required
              value={username}
              onChange={(event) => setUsername(event.target.value)}
            />
          </Field>
          <Field label="Password" htmlFor="login-password">
            <Input
              id="login-password"
              name="password"
              type="password"
              autoComplete="current-password"
              required
              value={password}
              onChange={(event) => setPassword(event.target.value)}
            />
          </Field>

          <Button type="submit" className="w-full" disabled={login.isPending}>
            {login.isPending ? 'Signing in…' : 'Sign in'}
          </Button>
        </form>
      </div>
    </div>
  )
}
