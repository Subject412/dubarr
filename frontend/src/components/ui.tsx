import type {
  ButtonHTMLAttributes,
  InputHTMLAttributes,
  ReactElement,
  ReactNode,
  SelectHTMLAttributes,
} from 'react'
import { cloneElement, isValidElement, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { cn } from '../lib/cn'
import { helpIdFor } from '../lib/help-id'

type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger'

type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: ButtonVariant
  size?: 'sm' | 'md'
}

const buttonVariants: Record<ButtonVariant, string> = {
  primary:
    'bg-accent-500 text-ink-950 hover:bg-accent-400 focus-visible:outline-accent-400 disabled:bg-accent-500/40',
  secondary:
    'bg-ink-700 text-zinc-100 hover:bg-ink-600 border border-ink-600 focus-visible:outline-ink-500',
  ghost: 'bg-transparent text-zinc-300 hover:bg-ink-800 hover:text-zinc-100',
  danger: 'bg-red-600 text-white hover:bg-red-500',
}

export function Button({ variant = 'primary', size = 'md', className, ...props }: ButtonProps) {
  return (
    <button
      className={cn(
        'inline-flex items-center justify-center gap-2 rounded-lg font-medium transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 disabled:cursor-not-allowed disabled:opacity-60',
        size === 'sm' ? 'px-3 py-1.5 text-sm' : 'px-4 py-2 text-sm',
        buttonVariants[variant],
        className,
      )}
      {...props}
    />
  )
}

export function Input({ className, ...props }: InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      className={cn(
        'w-full rounded-lg border border-ink-700 bg-ink-950/70 px-3 py-2 text-sm text-zinc-100 placeholder:text-zinc-500',
        'focus:border-accent-500 focus:outline-none disabled:opacity-60',
        className,
      )}
      {...props}
    />
  )
}

export function Select({ className, children, ...props }: SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <select
      className={cn(
        'w-full rounded-lg border border-ink-700 bg-ink-950/70 px-3 py-2 text-sm text-zinc-100',
        'focus:border-accent-500 focus:outline-none',
        className,
      )}
      {...props}
    >
      {children}
    </select>
  )
}

/**
 * Small "i" button with a hover/focus popover. Fixed positioning keeps the
 * tooltip out of the document flow (no layout shift) and clamped inside the
 * viewport; when there is no room above, it flips below the *control* it
 * documents so it never covers it.
 */
const CONTROL_SELECTOR = 'input, select, textarea, [role="switch"], [role="combobox"]'

/** The form control associated with a help id (shares the trigger's aria-describedby). */
function controlForHelpId(id: string, trigger: Element): Element | null {
  if (!id) return null
  for (const element of document.querySelectorAll('[aria-describedby]')) {
    if (element === trigger || !element.matches(CONTROL_SELECTOR)) continue
    const ids = (element.getAttribute('aria-describedby') ?? '').split(/\s+/)
    if (ids.includes(id)) return element
  }
  return null
}

export function InfoTip({ id, label, text }: { id: string; label: string; text: string }) {
  const [open, setOpen] = useState(false)
  const [position, setPosition] = useState<{ top: number; left: number } | null>(null)
  const triggerRef = useRef<HTMLButtonElement>(null)
  const tipRef = useRef<HTMLDivElement>(null)

  // Measure after the tooltip exists so the clamp uses its real width/height.
  useLayoutEffect(() => {
    if (!open) return
    const trigger = triggerRef.current
    const tip = tipRef.current
    if (!trigger || !tip) return
    const anchor = trigger.getBoundingClientRect()
    const size = tip.getBoundingClientRect()
    const margin = 8
    const viewportWidth = window.innerWidth || 1024
    const viewportHeight = window.innerHeight || 768
    const centered = anchor.left + anchor.width / 2 - size.width / 2
    const left = Math.min(
      Math.max(margin, centered),
      Math.max(margin, viewportWidth - size.width - margin),
    )
    // Prefer above the trigger. With no room there, flip below the control the
    // tooltip describes (the next element in the label row), not below the
    // trigger, which would cover that control. Always clamp to the viewport.
    const above = anchor.top - size.height - margin
    let top = above
    if (above < margin) {
      const control = controlForHelpId(trigger.getAttribute('aria-describedby') ?? '', trigger)
      const controlRect = control?.getBoundingClientRect()
      const base = controlRect && controlRect.height > 0 ? controlRect.bottom : anchor.bottom
      top = base + margin
    }
    const maxTop = Math.max(margin, viewportHeight - size.height - margin)
    setPosition({ top: Math.min(Math.max(margin, top), maxTop), left })
  }, [open])

  useEffect(() => {
    if (!open) return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') setOpen(false)
    }
    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [open])

  return (
    <span className="inline-flex align-middle">
      <button
        ref={triggerRef}
        type="button"
        aria-label={`About ${label}`}
        aria-describedby={id}
        onMouseEnter={() => setOpen(true)}
        onMouseLeave={() => setOpen(false)}
        onFocus={() => setOpen(true)}
        onBlur={() => setOpen(false)}
        onKeyDown={(event) => {
          if (event.key === 'Escape') setOpen(false)
        }}
        className="inline-flex h-4 w-4 items-center justify-center rounded-full border border-ink-600 text-[10px] font-semibold leading-none text-zinc-400 hover:border-accent-500 hover:text-accent-400 focus-visible:outline-2 focus-visible:outline-accent-400"
      >
        i
      </button>
      {open && (
        <div
          ref={tipRef}
          id={id}
          role="tooltip"
          style={{ position: 'fixed', top: position?.top ?? 0, left: position?.left ?? 0 }}
          className="z-50 w-72 max-w-[calc(100vw-1rem)] rounded-lg border border-ink-600 bg-ink-800 px-3 py-2 text-xs leading-relaxed text-zinc-200 shadow-xl"
        >
          {text}
        </div>
      )}
    </span>
  )
}

function FieldLabel({ label, htmlFor, help }: { label: string; htmlFor?: string; help?: string }) {
  return (
    <div className="flex items-center gap-1.5">
      <label htmlFor={htmlFor} className="block text-sm font-medium text-zinc-200">
        {label}
      </label>
      {help && <InfoTip id={helpIdFor(label)} label={label} text={help} />}
    </div>
  )
}

export function Field({
  label,
  hint,
  error,
  htmlFor,
  help,
  hidden = false,
  children,
}: {
  label: string
  hint?: string
  error?: string
  htmlFor?: string
  /** Long-form definition rendered in an info popover instead of inline text. */
  help?: string
  /**
   * A control that does not apply under the current settings must be absent,
   * not greyed out: render nothing at all (no label, no input, no disabled
   * placeholder) so it cannot be found or focused.
   */
  hidden?: boolean
  children: ReactNode
}) {
  if (hidden) return null

  const describedBy = help ? helpIdFor(label) : undefined
  const content =
    describedBy && isValidElement(children)
      ? cloneElement(children as ReactElement<{ 'aria-describedby'?: string }>, {
          'aria-describedby':
            (children.props as { 'aria-describedby'?: string })['aria-describedby'] ?? describedBy,
        })
      : children

  return (
    <div className="space-y-1.5">
      <FieldLabel label={label} htmlFor={htmlFor} help={help} />
      {content}
      {hint && !error && <p className="text-xs text-zinc-500">{hint}</p>}
      {error && (
        <p role="alert" className="text-xs text-red-400">
          {error}
        </p>
      )}
    </div>
  )
}

export function Card({
  id,
  title,
  description,
  actions,
  children,
  className,
}: {
  id?: string
  title?: ReactNode
  description?: ReactNode
  actions?: ReactNode
  children?: ReactNode
  className?: string
}) {
  return (
    <section
      id={id}
      className={cn('scroll-mt-6 rounded-xl border border-ink-800 bg-ink-900 shadow-sm', className)}
    >
      {(title || actions) && (
        <header className="flex flex-wrap items-start justify-between gap-3 border-b border-ink-800 px-5 py-4">
          <div>
            {title && <h2 className="text-base font-semibold text-zinc-100">{title}</h2>}
            {description && <p className="mt-1 text-sm text-zinc-400">{description}</p>}
          </div>
          {actions && <div className="flex items-center gap-2">{actions}</div>}
        </header>
      )}
      <div className="p-5">{children}</div>
    </section>
  )
}

type Tone = 'neutral' | 'success' | 'warning' | 'danger' | 'accent'

const badgeTones: Record<Tone, string> = {
  neutral: 'border-ink-600 bg-ink-800 text-zinc-300',
  success: 'border-emerald-700/60 bg-emerald-900/40 text-emerald-300',
  warning: 'border-amber-700/60 bg-amber-900/40 text-amber-300',
  danger: 'border-red-700/60 bg-red-900/40 text-red-300',
  accent: 'border-accent-500/40 bg-accent-500/10 text-accent-400',
}

export function Badge({ tone = 'neutral', children }: { tone?: Tone; children: ReactNode }) {
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1 rounded-full border px-2.5 py-0.5 text-xs font-medium',
        badgeTones[tone],
      )}
    >
      {children}
    </span>
  )
}

export function Toggle({
  checked,
  onChange,
  label,
  description,
  help,
  disabled,
  hidden = false,
}: {
  checked: boolean
  onChange: (next: boolean) => void
  label: string
  description?: string
  /** Long-form definition rendered in an info popover. */
  help?: string
  disabled?: boolean
  /** Inapplicable switches are absent from the DOM, never rendered disabled. */
  hidden?: boolean
}) {
  if (hidden) return null

  return (
    <div className="flex items-start justify-between gap-4 py-2">
      <div>
        <div className="flex items-center gap-1.5">
          <p className="text-sm font-medium text-zinc-200">{label}</p>
          {help && <InfoTip id={helpIdFor(label)} label={label} text={help} />}
        </div>
        {description && <p className="text-xs text-zinc-500">{description}</p>}
      </div>
      <button
        type="button"
        role="switch"
        aria-checked={checked}
        aria-label={label}
        aria-describedby={help ? helpIdFor(label) : undefined}
        disabled={disabled}
        onClick={() => onChange(!checked)}
        className={cn(
          'relative mt-0.5 h-6 w-11 shrink-0 rounded-full border transition-colors',
          checked ? 'border-accent-500 bg-accent-500/80' : 'border-ink-600 bg-ink-700',
          disabled && 'opacity-60',
        )}
      >
        <span
          className={cn(
            'absolute top-0.5 h-4.5 w-4.5 rounded-full bg-white transition-all',
            checked ? 'left-5.5' : 'left-0.5',
          )}
        />
      </button>
    </div>
  )
}

export function Alert({
  tone = 'danger',
  children,
}: {
  tone?: 'danger' | 'success' | 'warning' | 'info'
  children: ReactNode
}) {
  const tones = {
    danger: 'border-red-800/70 bg-red-950/40 text-red-300',
    success: 'border-emerald-800/70 bg-emerald-950/40 text-emerald-300',
    warning: 'border-amber-800/70 bg-amber-950/40 text-amber-300',
    info: 'border-accent-500/30 bg-accent-500/5 text-accent-400',
  }
  return (
    <div role="alert" className={cn('rounded-lg border px-3 py-2 text-sm', tones[tone])}>
      {children}
    </div>
  )
}

export function PageHeader({
  title,
  description,
  actions,
}: {
  title: string
  description?: string
  actions?: ReactNode
}) {
  return (
    <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight text-zinc-100">{title}</h1>
        {description && <p className="mt-1 text-sm text-zinc-400">{description}</p>}
      </div>
      {actions && <div className="flex items-center gap-2">{actions}</div>}
    </div>
  )
}

export function Spinner({ label = 'Loading' }: { label?: string }) {
  return (
    <div className="flex items-center gap-3 text-sm text-zinc-400" role="status">
      <span className="h-4 w-4 animate-spin rounded-full border-2 border-ink-600 border-t-accent-500" />
      {label}
    </div>
  )
}

/** One-click preset for a StringListEditor (e.g. a reserved language token). */
export type StringListPreset = {
  /** Stored value, e.g. a reserved language token. */
  value: string
  /** Friendly chip/preset label shown instead of the raw value. */
  label: string
}

/** Editable list of short strings (languages, filename tags). */
export function StringListEditor({
  label,
  values,
  onChange,
  placeholder,
  addLabel = 'Add',
  inputLabel,
  help,
  presets = [],
}: {
  label: string
  values: string[]
  onChange: (next: string[]) => void
  placeholder?: string
  addLabel?: string
  inputLabel: string
  /** Long-form definition rendered in an info popover. */
  help?: string
  /** One-click special entries (e.g. reserved language tokens). */
  presets?: StringListPreset[]
}) {
  const [text, setText] = useState('')

  const add = () => {
    const next = text.trim()
    if (next && !values.includes(next)) onChange([...values, next])
    setText('')
  }

  const labelFor = (value: string) => presets.find((preset) => preset.value === value)?.label ?? value
  const available = presets.filter((preset) => !values.includes(preset.value))

  return (
    <div className="space-y-2">
      <div className="flex items-center gap-1.5">
        <span className="block text-sm font-medium text-zinc-200">{label}</span>
        {help && <InfoTip id={helpIdFor(label)} label={label} text={help} />}
      </div>
      <div className="flex flex-wrap gap-2">
        {values.length === 0 && <span className="text-xs text-zinc-500">No entries yet.</span>}
        {values.map((value) => (
          <span
            key={value}
            className="inline-flex items-center gap-1 rounded-full border border-ink-600 bg-ink-800 px-2.5 py-1 text-xs text-zinc-200"
          >
            {labelFor(value)}
            <button
              type="button"
              aria-label={`Remove ${value}`}
              className="text-zinc-500 hover:text-red-400"
              onClick={() => onChange(values.filter((item) => item !== value))}
            >
              ×
            </button>
          </span>
        ))}
      </div>
      {available.length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {available.map((preset) => (
            <button
              key={preset.value}
              type="button"
              aria-label={preset.label}
              className="rounded-full border border-dashed border-ink-600 px-2.5 py-1 text-xs text-zinc-400 hover:border-accent-500 hover:text-accent-300"
              onClick={() => onChange([...values, preset.value])}
            >
              + {preset.label}
            </button>
          ))}
        </div>
      )}
      <div className="flex gap-2">
        <Input
          aria-label={inputLabel}
          aria-describedby={help ? helpIdFor(label) : undefined}
          placeholder={placeholder}
          value={text}
          onChange={(event) => setText(event.target.value)}
          onKeyDown={(event) => {
            if (event.key !== 'Enter') return
            event.preventDefault()
            add()
          }}
        />
        <Button type="button" variant="secondary" size="sm" onClick={add}>
          {addLabel}
        </Button>
      </div>
    </div>
  )
}
