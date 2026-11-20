import { useState } from 'react'
import { BrowseModal } from './BrowseModal'
import { Button, Field, Input } from './ui'

/** Text input plus a Browse button backed by the server directory picker. */
export function PathField({
  label,
  hint,
  value,
  onChange,
  placeholder,
}: {
  label: string
  hint?: string
  value: string
  onChange: (next: string) => void
  placeholder?: string
}) {
  const [open, setOpen] = useState(false)
  const id = `path-${label.replace(/\s+/g, '-').toLowerCase()}`
  return (
    <Field label={label} hint={hint} htmlFor={id}>
      <div className="flex gap-2">
        <Input
          id={id}
          aria-label={label}
          placeholder={placeholder ?? '/absolute/path'}
          value={value}
          onChange={(event) => onChange(event.target.value)}
        />
        <Button type="button" variant="secondary" size="sm" onClick={() => setOpen(true)}>
          Browse…
        </Button>
      </div>
      {open && (
        <BrowseModal
          open
          initialPath={value}
          title={`Select ${label}`}
          onClose={() => setOpen(false)}
          onSelect={(path) => {
            onChange(path)
            setOpen(false)
          }}
        />
      )}
    </Field>
  )
}

/** List of directory paths with add/remove and a browser-based picker. */
export function PathListEditor({
  label,
  values,
  onChange,
  hint,
}: {
  label: string
  values: string[]
  onChange: (next: string[]) => void
  hint?: string
}) {
  const [open, setOpen] = useState(false)
  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between">
        <span className="text-sm font-medium text-zinc-200">{label}</span>
        <Button type="button" variant="secondary" size="sm" onClick={() => setOpen(true)}>
          Add folder…
        </Button>
      </div>
      {hint && <p className="text-xs text-zinc-500">{hint}</p>}
      {values.length === 0 ? (
        <p className="text-xs text-zinc-500">No media roots configured.</p>
      ) : (
        <ul className="space-y-1">
          {values.map((path) => (
            <li
              key={path}
              className="flex items-center gap-2 rounded-lg border border-ink-700 bg-ink-950/50 px-3 py-1.5"
            >
              <span className="min-w-0 flex-1 truncate font-mono text-xs text-zinc-300">{path}</span>
              <button
                type="button"
                aria-label={`Remove ${path}`}
                className="text-zinc-500 hover:text-red-400"
                onClick={() => onChange(values.filter((item) => item !== path))}
              >
                ×
              </button>
            </li>
          ))}
        </ul>
      )}
      {open && (
        <BrowseModal
          open
          title={`Add ${label}`}
          onClose={() => setOpen(false)}
          onSelect={(path) => {
            if (!values.includes(path)) onChange([...values, path])
            setOpen(false)
          }}
        />
      )}
    </div>
  )
}
