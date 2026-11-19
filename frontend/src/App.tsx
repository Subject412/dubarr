import { NavLink, Route, Routes } from 'react-router-dom'

const navItems = [
  { to: '/', label: 'Dashboard', end: true },
  { to: '/library', label: 'Library' },
  { to: '/sync', label: 'Sync Studio' },
  { to: '/queue', label: 'Queue' },
  { to: '/settings', label: 'Settings' },
]

function Placeholder({ title }: { title: string }) {
  return (
    <section className="rounded-lg border border-ink-700 bg-ink-900 p-6">
      <h2 className="text-lg font-semibold text-slate-100">{title}</h2>
      <p className="mt-2 text-sm text-slate-400">
        Scaffold placeholder. This view is implemented in a later milestone.
      </p>
    </section>
  )
}

function App() {
  return (
    <div className="flex min-h-full flex-col">
      <header className="border-b border-ink-800 bg-ink-900">
        <div className="mx-auto flex max-w-6xl items-center gap-6 px-4 py-3">
          <span className="text-base font-semibold tracking-tight text-accent-400">
            dubarr
          </span>
          <nav className="flex gap-1 text-sm">
            {navItems.map((item) => (
              <NavLink
                key={item.to}
                to={item.to}
                end={item.end}
                className={({ isActive }) =>
                  [
                    'rounded px-3 py-1.5 transition-colors',
                    isActive
                      ? 'bg-ink-700 text-accent-400'
                      : 'text-slate-400 hover:bg-ink-800 hover:text-slate-200',
                  ].join(' ')
                }
              >
                {item.label}
              </NavLink>
            ))}
          </nav>
        </div>
      </header>

      <main className="mx-auto w-full max-w-6xl flex-1 px-4 py-8">
        <Routes>
          <Route index element={<Placeholder title="Dashboard" />} />
          <Route path="library" element={<Placeholder title="Library" />} />
          <Route path="sync" element={<Placeholder title="Sync Studio" />} />
          <Route path="queue" element={<Placeholder title="Queue" />} />
          <Route path="settings" element={<Placeholder title="Settings" />} />
          <Route path="*" element={<Placeholder title="Not found" />} />
        </Routes>
      </main>

      <footer className="border-t border-ink-800 px-4 py-4 text-center text-xs text-slate-600">
        Dubarr — self-hosted dubbing sync. M1 scaffold.
      </footer>
    </div>
  )
}

export default App
