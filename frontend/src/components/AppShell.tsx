import { NavLink, Outlet, useNavigate } from 'react-router-dom'
import { useLogout, useMe } from '../lib/hooks'
import { cn } from '../lib/cn'
import { Badge, Button } from './ui'

const navItems = [
  { to: '/library', label: 'Library', icon: '▤', hint: 'M2' },
  { to: '/sync', label: 'Sync Studio', icon: '◉', hint: 'M5' },
  { to: '/settings', label: 'Settings', icon: '⚙' },
  { to: '/system', label: 'System', icon: '♥' },
]

export function AppShell() {
  const navigate = useNavigate()
  const me = useMe()
  const logout = useLogout()

  const signOut = () => {
    logout.mutate(undefined, {
      onSettled: () => navigate('/login', { replace: true }),
    })
  }

  return (
    <div className="flex min-h-full bg-ink-950 text-zinc-200">
      <aside className="hidden w-60 shrink-0 flex-col border-r border-ink-800 bg-ink-900 md:flex">
        <div className="flex h-16 items-center gap-2 border-b border-ink-800 px-5">
          <span className="grid h-8 w-8 place-items-center rounded-lg bg-accent-500 font-bold text-ink-950">
            D
          </span>
          <div>
            <p className="text-sm font-semibold tracking-wide text-zinc-100">Dubarr</p>
            <p className="text-[11px] uppercase tracking-wider text-zinc-500">dubbing sync</p>
          </div>
        </div>
        <nav className="flex-1 space-y-1 p-3">
          {navItems.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              className={({ isActive }) =>
                cn(
                  'flex items-center gap-3 rounded-lg px-3 py-2 text-sm transition-colors',
                  isActive
                    ? 'bg-accent-500/10 text-accent-400'
                    : 'text-zinc-400 hover:bg-ink-800 hover:text-zinc-100',
                )
              }
            >
              <span aria-hidden className="w-4 text-center">
                {item.icon}
              </span>
              <span className="flex-1">{item.label}</span>
              {item.hint && (
                <span className="rounded border border-ink-700 px-1 text-[10px] text-zinc-500">
                  {item.hint}
                </span>
              )}
            </NavLink>
          ))}
        </nav>
        <div className="border-t border-ink-800 p-4 text-xs text-zinc-500">
          <p>M1 shell · self-hosted</p>
        </div>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-16 items-center justify-between gap-4 border-b border-ink-800 bg-ink-900/70 px-5 backdrop-blur">
          <div className="flex items-center gap-3">
            <span className="grid h-7 w-7 place-items-center rounded bg-accent-500 font-bold text-ink-950 md:hidden">
              D
            </span>
            <h1 className="text-sm font-semibold tracking-wide text-zinc-200">Dubarr</h1>
          </div>
          <div className="flex items-center gap-3">
            <Badge tone="neutral">{me.data?.username ?? 'signed in'}</Badge>
            {me.data?.is_admin && <Badge tone="accent">admin</Badge>}
            <Button variant="secondary" size="sm" onClick={signOut} disabled={logout.isPending}>
              {logout.isPending ? 'Signing out…' : 'Log out'}
            </Button>
          </div>
        </header>

        <nav className="flex gap-1 overflow-x-auto border-b border-ink-800 bg-ink-900 px-3 py-2 md:hidden">
          {navItems.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              className={({ isActive }) =>
                cn(
                  'whitespace-nowrap rounded-md px-3 py-1.5 text-xs',
                  isActive ? 'bg-accent-500/10 text-accent-400' : 'text-zinc-400',
                )
              }
            >
              {item.label}
            </NavLink>
          ))}
        </nav>

        <main className="flex-1 overflow-y-auto p-5 md:p-8">
          <div className="mx-auto w-full max-w-5xl">
            <Outlet />
          </div>
        </main>
      </div>
    </div>
  )
}
