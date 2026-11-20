import { useState } from 'react'
import { useBrowse } from '../lib/hooks'
import { Alert, Button, Input, Spinner } from './ui'

/**
 * Directory picker backed by GET /api/system/browse. Only directories are
 * selectable; files are rendered disabled to keep the choice unambiguous.
 */
export function BrowseModal({
  open,
  initialPath,
  title = 'Choose a directory',
  onClose,
  onSelect,
}: {
  open: boolean
  initialPath?: string
  title?: string
  onClose: () => void
  onSelect: (path: string) => void
}) {
  const [path, setPath] = useState(initialPath && initialPath.startsWith('/') ? initialPath : '/')
  const [manual, setManual] = useState('')

  const browse = useBrowse(open ? path : null)
  const data = browse.data

  if (!open) return null

  const go = (next: string) => {
    const trimmed = next.trim()
    if (!trimmed.startsWith('/')) return
    setPath(trimmed)
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
      <div
        role="dialog"
        aria-modal="true"
        aria-label={title}
        className="flex max-h-[85vh] w-full max-w-2xl flex-col rounded-xl border border-ink-700 bg-ink-900 shadow-2xl"
      >
        <header className="flex items-center justify-between border-b border-ink-800 px-5 py-3">
          <h2 className="text-sm font-semibold text-zinc-100">{title}</h2>
          <button
            type="button"
            aria-label="Close directory picker"
            className="text-zinc-400 hover:text-zinc-100"
            onClick={onClose}
          >
            ×
          </button>
        </header>

        <div className="space-y-3 border-b border-ink-800 px-5 py-3">
          <div className="flex gap-2">
            <Input
              aria-label="Absolute directory path"
              placeholder="/absolute/path"
              value={manual}
              onChange={(event) => setManual(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === 'Enter') {
                  event.preventDefault()
                  go(manual)
                }
              }}
            />
            <Button type="button" variant="secondary" size="sm" onClick={() => go(manual)}>
              Go
            </Button>
          </div>
          <div className="flex items-center gap-2 text-xs text-zinc-500">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              disabled={!data?.parent}
              onClick={() => data?.parent && setPath(data.parent)}
            >
              ↑ Up
            </Button>
            <span className="truncate font-mono text-zinc-400">{data?.path ?? path}</span>
          </div>
        </div>

        <div className="min-h-48 flex-1 overflow-y-auto px-2 py-2">
          {browse.isLoading && (
            <div className="p-4">
              <Spinner label="Reading directory" />
            </div>
          )}
          {browse.error && (
            <div className="p-4">
              <Alert>{browse.error.message}</Alert>
            </div>
          )}
          {data && data.entries.length === 0 && (
            <p className="p-4 text-sm text-zinc-500">Empty directory.</p>
          )}
          <ul>
            {data?.entries.map((entry) => (
              <li key={entry.path}>
                <button
                  type="button"
                  disabled={!entry.is_dir}
                  onClick={() => setPath(entry.path)}
                  className="flex w-full items-center gap-2 rounded-md px-3 py-2 text-left text-sm text-zinc-300 hover:bg-ink-800 disabled:cursor-not-allowed disabled:text-zinc-600"
                >
                  <span aria-hidden>{entry.is_dir ? '📁' : '📄'}</span>
                  <span className="truncate">{entry.name}</span>
                </button>
              </li>
            ))}
          </ul>
        </div>

        <footer className="flex items-center justify-between gap-3 border-t border-ink-800 px-5 py-3">
          <span className="truncate font-mono text-xs text-zinc-500">{data?.path ?? path}</span>
          <div className="flex gap-2">
            <Button type="button" variant="ghost" size="sm" onClick={onClose}>
              Cancel
            </Button>
            <Button
              type="button"
              size="sm"
              disabled={!data}
              onClick={() => {
                if (data) onSelect(data.path)
              }}
            >
              Select this folder
            </Button>
          </div>
        </footer>
      </div>
    </div>
  )
}
