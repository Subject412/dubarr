import { useState, type FormEvent } from 'react'
import { useChangePassword } from '../lib/hooks'
import { Alert, Button, Field, Input } from './ui'

/** Mirrors the server's minPasswordLen; the server still owns validation. */
const MIN_PASSWORD_LEN = 8

type FieldErrors = {
  current?: string
  next?: string
  confirm?: string
}

/**
 * Change-password dialog for the signed-in user. The browser validates first
 * so obvious mistakes never hit the API, but server errors are shown verbatim;
 * the current session stays valid, so success keeps the user where they are.
 * It is mounted only while open, so every opening starts from a clean form.
 */
export function ChangePasswordDialog({ onClose }: { onClose: () => void }) {
  const change = useChangePassword()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [errors, setErrors] = useState<FieldErrors>({})
  const [status, setStatus] = useState<{ tone: 'success' | 'danger'; message: string } | null>(null)

  const submit = (event: FormEvent) => {
    event.preventDefault()
    const found: FieldErrors = {}
    if (current === '') found.current = 'Current password is required.'
    if (next.length < MIN_PASSWORD_LEN) {
      found.next = `New password must be at least ${MIN_PASSWORD_LEN} characters.`
    } else if (next === current) {
      found.next = 'New password must differ from the current password.'
    }
    if (confirm !== next) found.confirm = 'Passwords do not match.'
    setErrors(found)
    setStatus(null)
    if (Object.keys(found).length > 0) return

    change.mutate(
      { currentPassword: current, newPassword: next },
      {
        onSuccess: () => {
          setCurrent('')
          setNext('')
          setConfirm('')
          setStatus({ tone: 'success', message: 'Password changed. Your other sessions were signed out.' })
        },
        onError: (error) => setStatus({ tone: 'danger', message: error.message }),
      },
    )
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
      <div
        role="dialog"
        aria-modal="true"
        aria-label="Change password"
        onKeyDown={(event) => {
          if (event.key === 'Escape') onClose()
        }}
        className="w-full max-w-sm rounded-xl border border-ink-700 bg-ink-900 shadow-2xl"
      >
        <header className="flex items-center justify-between border-b border-ink-800 px-5 py-3">
          <h2 className="text-sm font-semibold text-zinc-100">Change password</h2>
          <button
            type="button"
            aria-label="Close change password"
            className="text-zinc-400 hover:text-zinc-100"
            onClick={onClose}
          >
            ×
          </button>
        </header>

        <form noValidate className="space-y-4 px-5 py-4" onSubmit={submit}>
          {status && <Alert tone={status.tone}>{status.message}</Alert>}

          <Field label="Current password" htmlFor="change-password-current" error={errors.current}>
            <Input
              id="change-password-current"
              type="password"
              autoComplete="current-password"
              value={current}
              onChange={(event) => setCurrent(event.target.value)}
            />
          </Field>
          <Field
            label="New password"
            htmlFor="change-password-new"
            hint={`At least ${MIN_PASSWORD_LEN} characters, different from the current one.`}
            error={errors.next}
          >
            <Input
              id="change-password-new"
              type="password"
              autoComplete="new-password"
              value={next}
              onChange={(event) => setNext(event.target.value)}
            />
          </Field>
          <Field label="Confirm new password" htmlFor="change-password-confirm" error={errors.confirm}>
            <Input
              id="change-password-confirm"
              type="password"
              autoComplete="new-password"
              value={confirm}
              onChange={(event) => setConfirm(event.target.value)}
            />
          </Field>

          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" size="sm" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" size="sm" disabled={change.isPending}>
              {change.isPending ? 'Saving…' : 'Change password'}
            </Button>
          </div>
        </form>
      </div>
    </div>
  )
}
