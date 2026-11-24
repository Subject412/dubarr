import { useState } from 'react'
import type { FormEvent } from 'react'
import { Alert, Badge, Button, Card, Field, Input, PageHeader, Spinner } from '../components/ui'
import type { ApiToken, ApiTokenCreated } from '../lib/api'
import { useCreateToken, useRevokeToken, useTokens } from '../lib/hooks'

/** Backend timestamps are UTC `YYYY-MM-DD HH:MM:SS`; show minutes + explicit UTC. */
function formatTimestamp(text: string | null | undefined): string {
  if (!text) return 'Never'
  const [date, time] = text.split(' ')
  if (!date || !time) return text
  return `${date} ${time.slice(0, 5)} UTC`
}

function TokenRow({
  token,
  onRevoke,
  revoking,
}: {
  token: ApiToken
  onRevoke: (id: number) => void
  revoking: boolean
}) {
  const [confirming, setConfirming] = useState(false)

  return (
    <tr className="border-b border-ink-800/70 last:border-0">
      <td className="px-4 py-3 text-sm font-medium text-zinc-100">{token.name}</td>
      <td className="px-4 py-3">
        <span className="font-mono text-xs text-zinc-300">{token.prefix}</span>
        <span className="text-xs text-zinc-600">…</span>
      </td>
      <td className="whitespace-nowrap px-4 py-3 text-xs text-zinc-400">
        {formatTimestamp(token.created_at)}
      </td>
      <td className="whitespace-nowrap px-4 py-3 text-xs text-zinc-400">
        {formatTimestamp(token.last_used_at)}
      </td>
      <td className="px-4 py-3 text-right">
        {confirming ? (
          <div className="flex items-center justify-end gap-2">
            <span className="text-xs text-amber-300">Revoke this token?</span>
            <Button
              size="sm"
              variant="danger"
              disabled={revoking}
              onClick={() => {
                onRevoke(token.id)
                setConfirming(false)
              }}
            >
              Confirm revoke
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>
              Cancel
            </Button>
          </div>
        ) : (
          <Button
            size="sm"
            variant="ghost"
            aria-label={`Revoke ${token.name}`}
            onClick={() => setConfirming(true)}
          >
            Revoke
          </Button>
        )}
      </td>
    </tr>
  )
}

export function TokensPage() {
  const tokens = useTokens()
  const createToken = useCreateToken()
  const revokeToken = useRevokeToken()
  const [name, setName] = useState('')
  const [created, setCreated] = useState<ApiTokenCreated | null>(null)
  const [copied, setCopied] = useState(false)

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (!name.trim()) return
    createToken.mutate(name.trim(), {
      onSuccess: (token) => {
        setCreated(token)
        setCopied(false)
        setName('')
      },
    })
  }

  const copy = async () => {
    if (!created) return
    try {
      await navigator.clipboard.writeText(created.token)
      setCopied(true)
    } catch {
      // Clipboard may be unavailable (insecure context); the token stays visible.
    }
  }

  return (
    <div>
      <PageHeader
        title="API Tokens"
        description="Create tokens for CLI and automation clients. Tokens authenticate with the X-Api-Key header."
      />

      <div className="mb-4 rounded-lg border border-ink-800 bg-ink-900 px-4 py-3 text-xs text-zinc-400">
        Send the token on every request:{' '}
        <code className="rounded bg-ink-950 px-1.5 py-0.5 font-mono text-zinc-200">
          X-Api-Key: dub_…
        </code>
      </div>

      {created && (
        <div
          role="status"
          aria-label="New API token"
          className="mb-4 rounded-lg border border-emerald-800/70 bg-emerald-950/40 px-4 py-3"
        >
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="min-w-0 space-y-2">
              <p className="text-sm font-medium text-emerald-300">
                Token “{created.name}” created — copy it now. It will not be shown again.
              </p>
              <code className="block break-all rounded bg-ink-950 px-2 py-1.5 font-mono text-sm text-zinc-100">
                {created.token}
              </code>
              <p className="text-xs text-emerald-200/80">
                Store it somewhere safe. Only a masked prefix is kept on the server.
              </p>
            </div>
            <div className="flex shrink-0 items-center gap-2">
              <Button size="sm" variant="secondary" onClick={() => void copy()}>
                {copied ? 'Copied' : 'Copy token'}
              </Button>
              <Button size="sm" variant="ghost" onClick={() => setCreated(null)}>
                Dismiss
              </Button>
            </div>
          </div>
        </div>
      )}

      <div className="space-y-6">
        <Card title="Create token" description="Name it after the machine or integration that will use it.">
          <form onSubmit={submit} className="flex flex-wrap items-end gap-3">
            <div className="min-w-56 flex-1">
              <Field label="Token name" htmlFor="token-name">
                <Input
                  id="token-name"
                  value={name}
                  onChange={(event) => setName(event.target.value)}
                  placeholder="living-room-cli"
                  autoComplete="off"
                />
              </Field>
            </div>
            <Button type="submit" disabled={createToken.isPending || name.trim() === ''}>
              {createToken.isPending ? 'Creating…' : 'Create token'}
            </Button>
          </form>
          {createToken.error && (
            <div className="mt-3">
              <Alert>Could not create token: {createToken.error.message}</Alert>
            </div>
          )}
        </Card>

        <Card
          title="Existing tokens"
          description="Revoking a token immediately breaks clients still using it."
          actions={tokens.data && <Badge tone="neutral">{tokens.data.tokens.length} total</Badge>}
        >
          {tokens.isLoading && <Spinner label="Loading tokens" />}
          {tokens.error && <Alert>Could not load tokens: {tokens.error.message}</Alert>}
          {revokeToken.error && <Alert>Could not revoke token: {revokeToken.error.message}</Alert>}

          {tokens.data && (
            <div className="-mx-5 -mb-5 overflow-x-auto">
              <table className="w-full min-w-[36rem] text-left">
                <thead>
                  <tr className="border-b border-ink-800 text-[11px] uppercase tracking-wider text-zinc-500">
                    <th className="px-4 pb-2 font-medium">Name</th>
                    <th className="px-4 pb-2 font-medium">Prefix</th>
                    <th className="px-4 pb-2 font-medium">Created</th>
                    <th className="px-4 pb-2 font-medium">Last used</th>
                    <th className="px-4 pb-2 text-right font-medium">Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {tokens.data.tokens.length === 0 ? (
                    <tr>
                      <td colSpan={5} className="px-4 py-6 text-center text-sm text-zinc-500">
                        No API tokens yet. Create one above for CLI or automation access.
                      </td>
                    </tr>
                  ) : (
                    tokens.data.tokens.map((token) => (
                      <TokenRow
                        key={token.id}
                        token={token}
                        revoking={revokeToken.isPending}
                        onRevoke={(id) => revokeToken.mutate(id)}
                      />
                    ))
                  )}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      </div>
    </div>
  )
}
