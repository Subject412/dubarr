import { useEffect, useRef, useState } from 'react'
import { Link, NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom'
import { useLogout, useMe } from '../lib/hooks'
import { cn } from '../lib/cn'
import {
  isSettingsSectionKey,
  SETTINGS_SECTIONS,
  type SettingsSection,
} from '../lib/settings-sections'
import { Badge } from './ui'
import { ChangePasswordDialog } from './ChangePasswordDialog'

type NavChild = {
  to: string
  label: string
  section: SettingsSection['key']
}

type NavItem = {
  to: string
  label: string
  icon: string
  hint?: string
  admin?: boolean
  children?: NavChild[]
}

const navItems: NavItem[] = [
  { to: '/queue', label: 'Approval Queue', icon: '✓', hint: 'M2' },
  { to: '/vault', label: 'Audio Vault', icon: '♫', hint: 'M3' },
  { to: '/library', label: 'Library', icon: '▤', hint: 'M2' },
  { to: '/sync', label: 'Sync Studio', icon: '◉', hint: 'M5' },
  {
    to: '/settings',
    label: 'Settings',
    icon: '⚙',
    // Generated from the same registry the Settings page renders, so the
    // submenu can never offer a category that has no section card.
    children: SETTINGS_SECTIONS.map((section) => ({
      to: `/settings?section=${section.key}`,
      label: section.title,
      section: section.key,
    })),
  },
  { to: '/system', label: 'System', icon: '♥' },
  { to: '/logs', label: 'Logs', icon: '≡', admin: true },
  { to: '/tokens', label: 'API Tokens', icon: '⚿', admin: true },
]

export function AppShell() {
  const navigate = useNavigate()
  const me = useMe()
  const logout = useLogout()
  const location = useLocation()
  const visibleNav = navItems.filter((item) => !item.admin || me.data?.is_admin)

  const onSettings = location.pathname === '/settings'
  const sectionParam = new URLSearchParams(location.search).get('section')
  const activeSection = isSettingsSectionKey(sectionParam) ? sectionParam : null

  // `null` follows the route (expanded on /settings, collapsed elsewhere);
  // the chevron overrides it until the next top-level navigation.
  const [manuallyExpanded, setManuallyExpanded] = useState<boolean | null>(null)
  const settingsExpanded = manuallyExpanded ?? onSettings

  // Account menu: the username badge is its trigger.
  const [menuOpen, setMenuOpen] = useState(false)
  const [passwordOpen, setPasswordOpen] = useState(false)
  const menuRef = useRef<HTMLDivElement>(null)

  // Close on outside click and on Escape, as the menu has no backdrop.
  useEffect(() => {
    if (!menuOpen) return
    const onPointerDown = (event: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(event.target as Node)) setMenuOpen(false)
    }
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') setMenuOpen(false)
    }
    document.addEventListener('mousedown', onPointerDown)
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('mousedown', onPointerDown)
      document.removeEventListener('keydown', onKeyDown)
    }
  }, [menuOpen])

  const signOut = () => {
    logout.mutate(undefined, {
      onSettled: () => navigate('/login', { replace: true }),
    })
  }

  return (
    <div className="flex min-h-full bg-ink-950 text-zinc-200">
      <aside className="hidden w-60 shrink-0 flex-col border-r border-ink-800 bg-ink-900 md:sticky md:top-0 md:h-dvh md:flex md:self-start">
        <div className="flex h-16 items-center gap-2 border-b border-ink-800 px-5">
          <span className="grid h-8 w-8 place-items-center rounded-lg bg-accent-500 font-bold text-ink-950">
            D
          </span>
          <div>
            <p className="text-sm font-semibold tracking-wide text-zinc-100">Dubarr</p>
            <p className="text-[11px] uppercase tracking-wider text-zinc-500">dubbing sync</p>
          </div>
        </div>
        <nav className="min-h-0 flex-1 space-y-1 overflow-y-auto p-3">
          {visibleNav.map((item) => {
            const hasChildren = Boolean(item.children?.length)
            const expanded = hasChildren && settingsExpanded
            return (
              <div key={item.to}>
                <div className="flex items-center gap-1">
                  <NavLink
                    to={item.to}
                    onClick={() => setManuallyExpanded(null)}
                    className={({ isActive }) =>
                      cn(
                        'flex flex-1 items-center gap-3 rounded-lg px-3 py-2 text-sm transition-colors',
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
                  {hasChildren && (
                    <button
                      type="button"
                      aria-label="Settings categories"
                      aria-expanded={expanded}
                      aria-controls="settings-submenu"
                      onClick={() => setManuallyExpanded(!expanded)}
                      className="rounded-md px-1.5 py-2 text-xs text-zinc-500 hover:bg-ink-800 hover:text-zinc-200"
                    >
                      <span aria-hidden className={cn('inline-block transition-transform', expanded && 'rotate-180')}>
                        ▾
                      </span>
                    </button>
                  )}
                </div>
                {hasChildren && (
                  <ul
                    id="settings-submenu"
                    hidden={!expanded}
                    className="mt-1 space-y-0.5 border-l border-ink-800 pl-3"
                  >
                    {item.children?.map((child) => (
                      <li key={child.to}>
                        <Link
                          to={child.to}
                          aria-current={activeSection === child.section ? 'page' : undefined}
                          className={cn(
                            'block rounded-md px-3 py-1.5 text-xs transition-colors',
                            activeSection === child.section
                              ? 'bg-accent-500/10 text-accent-400'
                              : 'text-zinc-500 hover:bg-ink-800 hover:text-zinc-100',
                          )}
                        >
                          {child.label}
                        </Link>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            )
          })}
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
            <div className="relative" ref={menuRef}>
              <button
                type="button"
                aria-haspopup="menu"
                aria-expanded={menuOpen}
                aria-controls="account-menu"
                onClick={() => setMenuOpen((open) => !open)}
                className="inline-flex items-center gap-1 rounded-full border border-ink-600 bg-ink-800 px-2.5 py-0.5 text-xs font-medium text-zinc-300 hover:bg-ink-700 hover:text-zinc-100 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent-400"
              >
                {me.data?.username ?? 'signed in'}
                <span aria-hidden className={cn('text-[10px] transition-transform', menuOpen && 'rotate-180')}>
                  ▾
                </span>
              </button>
              {menuOpen && (
                <div
                  id="account-menu"
                  role="menu"
                  aria-label="Account menu"
                  className="absolute right-0 z-40 mt-2 w-48 rounded-lg border border-ink-700 bg-ink-800 p-1 shadow-2xl"
                >
                  <button
                    type="button"
                    role="menuitem"
                    className="block w-full rounded-md px-3 py-2 text-left text-sm text-zinc-300 hover:bg-ink-700 hover:text-zinc-100"
                    onClick={() => {
                      setMenuOpen(false)
                      setPasswordOpen(true)
                    }}
                  >
                    Change password
                  </button>
                  <button
                    type="button"
                    role="menuitem"
                    disabled={logout.isPending}
                    className="block w-full rounded-md px-3 py-2 text-left text-sm text-zinc-300 hover:bg-ink-700 hover:text-zinc-100 disabled:opacity-60"
                    onClick={() => {
                      setMenuOpen(false)
                      signOut()
                    }}
                  >
                    {logout.isPending ? 'Signing out…' : 'Log out'}
                  </button>
                </div>
              )}
            </div>
            {me.data?.is_admin && <Badge tone="accent">Administrator</Badge>}
          </div>
        </header>

        <nav className="flex gap-1 overflow-x-auto border-b border-ink-800 bg-ink-900 px-3 py-2 md:hidden">
          {visibleNav.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              onClick={() => setManuallyExpanded(null)}
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

        {onSettings && (
          <nav
            aria-label="Settings categories"
            className="flex gap-1 overflow-x-auto border-b border-ink-800 bg-ink-900 px-3 pb-2 md:hidden"
          >
            {SETTINGS_SECTIONS.map((section) => (
              <Link
                key={section.key}
                to={`/settings?section=${section.key}`}
                aria-current={activeSection === section.key ? 'page' : undefined}
                className={cn(
                  'whitespace-nowrap rounded-full border px-3 py-1 text-xs',
                  activeSection === section.key
                    ? 'border-accent-500/40 bg-accent-500/10 text-accent-400'
                    : 'border-ink-700 text-zinc-400',
                )}
              >
                {section.title}
              </Link>
            ))}
          </nav>
        )}

        <main className="flex-1 overflow-y-auto p-5 md:p-8">
          <div className="mx-auto w-full max-w-5xl">
            <Outlet />
          </div>
        </main>
      </div>

      {passwordOpen && <ChangePasswordDialog onClose={() => setPasswordOpen(false)} />}
    </div>
  )
}
