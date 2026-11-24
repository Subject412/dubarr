import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { LogsPage } from '../pages/Logs'
import { formatRelative } from '../lib/logs-format'
import type { LogEntry } from '../lib/api'
import { isMethod, jsonResponse, renderWithProviders } from './helpers'

function entry(level: LogEntry['level'], component: string, message: string, ageMs: number): LogEntry {
  return {
    ts: new Date(Date.now() - ageMs).toISOString(),
    level,
    component,
    message,
  }
}

const ENTRIES: LogEntry[] = [
  entry('info', 'watch', 'scanner started', 5_000),
  entry('warn', 'syncworker', 'job 7 slow', 65_000),
  entry('error', 'arrsync', 'arr unreachable', 3_700_000),
]

type Handler = (init?: RequestInit) => Response | Promise<Response>

function logsFetch({
  entries = ENTRIES,
  level = 'info',
  overrides = {},
}: {
  entries?: LogEntry[]
  level?: string
  overrides?: Record<string, Handler>
} = {}) {
  let currentLevel = level
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    for (const [fragment, handler] of Object.entries(overrides)) {
      if (url.includes(fragment)) return handler(init)
    }
    if (url.includes('/api/logs/level') && isMethod(init, 'PUT')) {
      const body = JSON.parse(String(init?.body)) as { level: string }
      currentLevel = body.level
      return jsonResponse({ level: currentLevel })
    }
    if (url.includes('/api/logs/level') && isMethod(init, 'GET')) {
      return jsonResponse({ level: currentLevel })
    }
    if (url.includes('/api/logs')) {
      const parsed = new URL(url, 'http://localhost')
      const min = parsed.searchParams.get('level')
      const rank: Record<string, number> = { debug: 0, info: 1, warn: 2, error: 3 }
      const filtered = min ? entries.filter((e) => rank[e.level] >= rank[min]) : entries
      return jsonResponse({ entries: filtered, level: currentLevel })
    }
    return jsonResponse({ error: 'unexpected' }, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('log timestamps', () => {
  const now = new Date('2026-01-02T12:00:00Z')

  it('formats relative ages and falls back to a date for old entries', () => {
    expect(formatRelative('2026-01-02T11:59:50Z', now)).toBe('10s ago')
    expect(formatRelative('2026-01-02T11:57:00Z', now)).toBe('3m ago')
    expect(formatRelative('2026-01-02T09:00:00Z', now)).toBe('3h ago')
    expect(formatRelative('2025-12-28T12:00:00Z', now)).toContain('2025')
  })
})

describe('logs page', () => {
  it('renders entries with level, component and message', async () => {
    logsFetch()
    renderWithProviders(<LogsPage />)

    expect(await screen.findByText('scanner started')).toBeInTheDocument()
    expect(screen.getByText('job 7 slow')).toBeInTheDocument()
    expect(screen.getByText('arr unreachable')).toBeInTheDocument()
    const list = screen.getByRole('list', { name: /log entries/i })
    expect(within(list).getAllByRole('listitem')).toHaveLength(3)
    expect(within(list).getByText('syncworker')).toBeInTheDocument()
  })

  it('filters by level chip server-side', async () => {
    const fetchMock = logsFetch()
    const user = userEvent.setup()
    renderWithProviders(<LogsPage />)
    await screen.findByText('scanner started')

    await user.click(screen.getByRole('button', { name: 'Warn' }))

    await waitFor(() => {
      expect(screen.queryByText('scanner started')).not.toBeInTheDocument()
    })
    expect(screen.getByText('job 7 slow')).toBeInTheDocument()
    expect(screen.getByText('arr unreachable')).toBeInTheDocument()
    expect(
      fetchMock.mock.calls.some(([input]) => String(input).includes('level=warn')),
    ).toBe(true)
  })

  it('filters by text search', async () => {
    logsFetch()
    const user = userEvent.setup()
    renderWithProviders(<LogsPage />)
    await screen.findByText('scanner started')

    await user.type(screen.getByLabelText(/search/i), 'arr')

    expect(screen.getByText('arr unreachable')).toBeInTheDocument()
    expect(screen.queryByText('scanner started')).not.toBeInTheDocument()
  })

  it('filters by component', async () => {
    logsFetch()
    const user = userEvent.setup()
    renderWithProviders(<LogsPage />)
    await screen.findByText('scanner started')

    await user.selectOptions(screen.getByLabelText(/component/i), 'syncworker')

    expect(screen.getByText('job 7 slow')).toBeInTheDocument()
    expect(screen.queryByText('scanner started')).not.toBeInTheDocument()
  })

  it('shows an empty state when no entry matches', async () => {
    logsFetch()
    const user = userEvent.setup()
    renderWithProviders(<LogsPage />)
    await screen.findByText('scanner started')

    await user.type(screen.getByLabelText(/search/i), 'nothing-matches-this')

    expect(screen.getByText(/no log entries match/i)).toBeInTheDocument()
  })

  it('pauses and resumes the live tail', async () => {
    logsFetch()
    const user = userEvent.setup()
    renderWithProviders(<LogsPage />)
    await screen.findByText('scanner started')

    await user.click(screen.getByRole('button', { name: /pause/i }))
    expect(screen.getByRole('button', { name: /resume/i })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /resume/i }))
    expect(screen.getByRole('button', { name: /pause/i })).toBeInTheDocument()
  })

  it('shows a clear error state when the sink is unavailable', async () => {
    logsFetch({
      overrides: {
        '/api/logs/level': () => jsonResponse({ level: 'info' }),
        '/api/logs': () => jsonResponse({ error: 'the log sink is unavailable' }, 503),
      },
    })
    renderWithProviders(<LogsPage />)

    expect(await screen.findByText(/could not load logs/i)).toBeInTheDocument()
    expect(screen.getByText(/log sink is unavailable/i)).toBeInTheDocument()
  })

  it('changes the runtime log level', async () => {
    const fetchMock = logsFetch()
    const user = userEvent.setup()
    renderWithProviders(<LogsPage />)
    await screen.findByText('scanner started')

    await user.selectOptions(screen.getByLabelText(/runtime log level/i), 'debug')

    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(
          ([input, init]) => String(input).includes('/api/logs/level') && isMethod(init, 'PUT'),
        ),
      ).toBe(true)
    })
  })

  it('copies the visible entries to the clipboard', async () => {
    logsFetch()
    const user = userEvent.setup()
    const writeText = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
    renderWithProviders(<LogsPage />)
    await screen.findByText('scanner started')

    await user.click(screen.getByRole('button', { name: /copy/i }))

    await waitFor(() => expect(writeText).toHaveBeenCalled())
    const copied = String(writeText.mock.calls[0][0])
    expect(copied).toContain('scanner started')
    expect(copied).toContain('arr unreachable')
  })
})
