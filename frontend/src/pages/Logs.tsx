import { useEffect, useMemo, useRef, useState } from 'react'
import { Alert, Badge, Button, Card, Input, PageHeader, Select, Spinner } from '../components/ui'
import type { LogLevel } from '../lib/api'
import { useLogLevel, useLogs, useSetLogLevel } from '../lib/hooks'
import { formatLogEntry, formatRelative } from '../lib/logs-format'

const LOG_LEVELS: LogLevel[] = ['debug', 'info', 'warn', 'error']

type LevelFilter = 'all' | LogLevel

const badgeTone: Record<LogLevel, 'neutral' | 'accent' | 'warning' | 'danger'> = {
  debug: 'neutral',
  info: 'accent',
  warn: 'warning',
  error: 'danger',
}

export function LogsPage() {
  const [level, setLevel] = useState<LevelFilter>('all')
  const [component, setComponent] = useState('')
  const [search, setSearch] = useState('')
  const [live, setLive] = useState(true)
  const [autoScroll, setAutoScroll] = useState(true)
  const [copied, setCopied] = useState(false)

  const logs = useLogs(
    {
      level: level === 'all' ? undefined : level,
      limit: 500,
    },
    live,
  )
  const logLevel = useLogLevel()
  const setLogLevel = useSetLogLevel()

  const entries = useMemo(() => logs.data?.entries ?? [], [logs.data])

  const components = useMemo(() => {
    const seen = new Set<string>()
    for (const entry of entries) {
      if (entry.component) seen.add(entry.component)
    }
    return [...seen].sort((a, b) => a.localeCompare(b))
  }, [entries])

  const visible = useMemo(() => {
    const needle = search.trim().toLowerCase()
    return entries.filter((entry) => {
      if (component && entry.component !== component) return false
      if (!needle) return true
      return (
        entry.message.toLowerCase().includes(needle) ||
        (entry.component ?? '').toLowerCase().includes(needle) ||
        entry.level.includes(needle)
      )
    })
  }, [entries, component, search])

  const tailRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!autoScroll) return
    tailRef.current?.scrollIntoView?.({ block: 'end' })
  }, [visible.length, autoScroll])

  const copyVisible = async () => {
    if (visible.length === 0) return
    try {
      await navigator.clipboard.writeText(visible.map(formatLogEntry).join('\n'))
      setCopied(true)
    } catch {
      // Clipboard may be unavailable (insecure context); nothing else to do.
    }
  }

  const downloadVisible = () => {
    if (visible.length === 0) return
    try {
      const blob = new Blob([visible.map(formatLogEntry).join('\n') + '\n'], {
        type: 'text/plain;charset=utf-8',
      })
      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url
      link.download = 'dubarr-logs.txt'
      link.click()
      URL.revokeObjectURL(url)
    } catch {
      // Download is a convenience; the visible tail stays on screen.
    }
  }

  return (
    <div>
      <PageHeader
        title="Logs"
        description="Application log tail from the in-memory ring. When Dubarr runs in Docker, the same lines are also on the container's stdout."
      />

      <Card
        title="Application logs"
        description="Poll every two seconds while live. Copy or download the filtered tail for a bug report."
        actions={
          <div className="flex flex-wrap items-center gap-2">
            <label htmlFor="log-runtime-level" className="text-xs text-zinc-500">
              Runtime log level
            </label>
            <Select
              id="log-runtime-level"
              aria-label="Runtime log level"
              className="w-28"
              value={logLevel.data?.level ?? 'info'}
              disabled={setLogLevel.isPending}
              onChange={(event) => setLogLevel.mutate(event.target.value as LogLevel)}
            >
              {LOG_LEVELS.map((lvl) => (
                <option key={lvl} value={lvl}>
                  {lvl}
                </option>
              ))}
            </Select>
          </div>
        }
      >
        <div className="mb-4 flex flex-wrap items-center gap-2">
          <Button
            size="sm"
            variant={live ? 'secondary' : 'primary'}
            onClick={() => setLive((current) => !current)}
          >
            {live ? 'Pause' : 'Resume'}
          </Button>
          <Button size="sm" variant="ghost" aria-pressed={autoScroll} onClick={() => setAutoScroll((v) => !v)}>
            Auto-scroll {autoScroll ? 'on' : 'off'}
          </Button>
          <Button size="sm" variant="ghost" disabled={visible.length === 0} onClick={() => void copyVisible()}>
            {copied ? 'Copied' : 'Copy visible'}
          </Button>
          <Button size="sm" variant="ghost" disabled={visible.length === 0} onClick={downloadVisible}>
            Download
          </Button>
        </div>

        <div className="mb-4 flex flex-wrap items-end gap-3">
          <div className="flex flex-wrap gap-1" role="group" aria-label="Filter by level">
            {(['all', ...LOG_LEVELS] as LevelFilter[]).map((lvl) => (
              <Button
                key={lvl}
                size="sm"
                variant={level === lvl ? 'primary' : 'ghost'}
                aria-pressed={level === lvl}
                onClick={() => setLevel(lvl)}
              >
                {lvl === 'all' ? 'All' : lvl[0].toUpperCase() + lvl.slice(1)}
              </Button>
            ))}
          </div>
          <div className="w-44">
            <Select
              aria-label="Component"
              value={component}
              onChange={(event) => setComponent(event.target.value)}
            >
              <option value="">All components</option>
              {components.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </Select>
          </div>
          <div className="min-w-56 flex-1">
            <Input
              aria-label="Search"
              value={search}
              placeholder="Search messages, components or levels"
              onChange={(event) => setSearch(event.target.value)}
            />
          </div>
        </div>

        {setLogLevel.error && (
          <div className="mb-3">
            <Alert>Could not change the log level: {setLogLevel.error.message}</Alert>
          </div>
        )}
        {logs.isPending && <Spinner label="Loading logs" />}
        {logs.error && <Alert>Could not load logs: {logs.error.message}</Alert>}

        {logs.data && visible.length === 0 && (
          <p className="py-8 text-center text-sm text-zinc-500">
            No log entries match the current filters.
          </p>
        )}

        {visible.length > 0 && (
          <div className="max-h-[32rem] overflow-y-auto rounded-lg border border-ink-800 bg-ink-950/60">
            <ol aria-label="Log entries" className="divide-y divide-ink-800/60 font-mono text-xs">
              {visible.map((entry, index) => (
                <li
                  key={`${entry.ts}-${index}`}
                  className="flex flex-wrap items-baseline gap-x-3 gap-y-1 px-3 py-2"
                >
                  <time
                    dateTime={entry.ts}
                    title={entry.ts}
                    className="w-16 shrink-0 text-zinc-500"
                  >
                    {formatRelative(entry.ts)}
                  </time>
                  <Badge tone={badgeTone[entry.level]}>{entry.level}</Badge>
                  {entry.component && (
                    <span className="text-accent-400">{entry.component}</span>
                  )}
                  <span className="min-w-0 flex-1 break-words text-zinc-200">{entry.message}</span>
                  {entry.job_id && <span className="text-zinc-500">job {entry.job_id}</span>}
                  {entry.media_id && <span className="text-zinc-500">media {entry.media_id}</span>}
                </li>
              ))}
            </ol>
            <div ref={tailRef} />
          </div>
        )}
      </Card>
    </div>
  )
}
