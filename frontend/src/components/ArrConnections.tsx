import { useEffect, useRef, useState } from 'react'
import type {
  ArrConnection,
  ArrConnectionKind,
  ArrConnectionPayload,
  ArrConnectionTestPayload,
} from '../lib/api'
import {
  arrErrorMessage,
  arrKindLabel,
  maskedArrKeyLabel,
  validateArrBaseUrl,
} from '../lib/arr-connections'
import {
  useArrConnections,
  useDeleteArrConnection,
  useSaveArrConnection,
  useTestArrConnection,
} from '../lib/hooks'
import { settingsHelp } from '../lib/settings-help'
import { settingsSection, settingsSectionId } from '../lib/settings-sections'
import { Alert, Badge, Button, Card, Field, Input, Toggle } from './ui'

const ARR_KINDS: readonly ArrConnectionKind[] = ['radarr', 'sonarr']

type Notice = { tone: 'success' | 'danger'; text: string }

/**
 * One Radarr or Sonarr connection. The API key field is write-only: it starts
 * empty on every mount, is cleared after a save, and the stored key is only
 * ever displayed as the server's mask. Leaving it blank keeps the saved key.
 */
function ArrConnectionCard({
  kind,
  connection,
}: {
  kind: ArrConnectionKind
  connection?: ArrConnection
}) {
  const label = arrKindLabel(kind)
  const save = useSaveArrConnection()
  const remove = useDeleteArrConnection()
  const test = useTestArrConnection()

  const [baseUrl, setBaseUrl] = useState(connection?.base_url ?? '')
  const [apiKey, setApiKey] = useState('')
  const [enabled, setEnabled] = useState(connection?.enabled ?? true)
  const [baseUrlError, setBaseUrlError] = useState('')
  const [notice, setNotice] = useState<Notice | null>(null)
  const [dirty, setDirty] = useState(false)
  const lastConnection = useRef(connection)

  // A save/delete refetches the list and hands the card a new object. Adopt the
  // server values unless the operator has unsaved edits in this card.
  // oxlint-disable react/set-state-in-effect
  useEffect(() => {
    if (lastConnection.current === connection) return
    lastConnection.current = connection
    if (dirty) return
    setBaseUrl(connection?.base_url ?? '')
    setEnabled(connection?.enabled ?? true)
    setApiKey('')
  }, [connection, dirty])
  // oxlint-enable react/set-state-in-effect

  const onSave = () => {
    setNotice(null)
    const check = validateArrBaseUrl(baseUrl)
    if (!check.ok) {
      setBaseUrlError(check.error)
      return
    }
    setBaseUrlError('')
    if (!connection && apiKey === '') {
      setNotice({ tone: 'danger', text: 'API key is required when adding a connection.' })
      return
    }
    const payload: ArrConnectionPayload = { base_url: baseUrl.trim(), enabled }
    // A blank field must not overwrite the stored secret; omitting the field
    // leaves the server-side key untouched.
    if (apiKey !== '') payload.api_key = apiKey
    save.mutate(
      { kind, payload },
      {
        onSuccess: () => {
          setDirty(false)
          setApiKey('')
          setNotice({ tone: 'success', text: `Saved ${label} connection.` })
        },
        onError: (error) => setNotice({ tone: 'danger', text: arrErrorMessage(error) }),
      },
    )
  }

  const onTest = () => {
    setNotice(null)
    const check = validateArrBaseUrl(baseUrl)
    if (!check.ok) {
      setBaseUrlError(check.error)
      return
    }
    setBaseUrlError('')
    // Test the draft, not the saved config: the form values are sent as the
    // ping target and the server persists none of them. A blank (or
    // whitespace-only) key field omits `api_key`, which tells the server to use
    // the stored key.
    const payload: ArrConnectionTestPayload = { base_url: baseUrl.trim() }
    if (apiKey.trim() !== '') payload.api_key = apiKey
    test.mutate(
      { kind, payload },
      {
        onSuccess: () =>
          setNotice({ tone: 'success', text: `${label} accepted the entered connection.` }),
        onError: (error) => setNotice({ tone: 'danger', text: arrErrorMessage(error) }),
      },
    )
  }

  const onRemove = () => {
    setNotice(null)
    remove.mutate(kind, {
      onSuccess: () => {
        setDirty(false)
        setBaseUrl('')
        setApiKey('')
        setEnabled(true)
        setNotice({ tone: 'success', text: `Removed ${label} connection.` })
      },
      onError: (error) => setNotice({ tone: 'danger', text: arrErrorMessage(error) }),
    })
  }

  return (
    <div
      role="group"
      aria-label={`${label} connection`}
      className="space-y-3 rounded-lg border border-ink-800 bg-ink-950/40 p-4"
    >
      <div className="flex items-center justify-between gap-3">
        <h3 className="text-sm font-semibold text-zinc-100">{label}</h3>
        {connection ? (
          <Badge tone={connection.enabled ? 'success' : 'neutral'}>
            {connection.enabled ? 'enabled' : 'disabled'}
          </Badge>
        ) : (
          <Badge tone="neutral">not configured</Badge>
        )}
      </div>

      <p className="text-xs text-zinc-500">
        {connection && connection.api_key !== '' ? (
          <>
            Stored API key: <code className="text-zinc-300">{maskedArrKeyLabel(connection.api_key)}</code>
            {' · '}leave the field blank to keep it.
          </>
        ) : (
          'No API key saved yet.'
        )}
      </p>

      <Field
        label="Base URL"
        hint="Include the scheme, e.g. http://192.168.1.10:7878."
        error={baseUrlError}
        help={settingsHelp('arr.connections', `${kind}_base_url`)}
        htmlFor={`arr-${kind}-base-url`}
      >
        <Input
          id={`arr-${kind}-base-url`}
          type="text"
          spellCheck={false}
          placeholder="http://radarr.local:7878"
          value={baseUrl}
          onChange={(event) => {
            setDirty(true)
            setBaseUrl(event.target.value)
          }}
        />
      </Field>

      <Field label="API key" help={settingsHelp('arr.connections', `${kind}_api_key`)} htmlFor={`arr-${kind}-api-key`}>
        <Input
          id={`arr-${kind}-api-key`}
          type="password"
          autoComplete="off"
          value={apiKey}
          onChange={(event) => {
            setDirty(true)
            setApiKey(event.target.value)
          }}
        />
      </Field>

      <Toggle
        label="Enabled"
        help={settingsHelp('arr.connections', `${kind}_enabled`)}
        checked={enabled}
        onChange={(next) => {
          setDirty(true)
          setEnabled(next)
        }}
      />

      {notice && <Alert tone={notice.tone}>{notice.text}</Alert>}

      <p className="text-right text-xs text-zinc-500">Tests the values above; saving is separate.</p>

      <div className="flex flex-wrap justify-end gap-2">
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={onTest}
          disabled={test.isPending || baseUrl.trim() === ''}
        >
          {test.isPending ? 'Testing…' : 'Test entered connection'}
        </Button>
        {connection && (
          <Button type="button" variant="danger" size="sm" onClick={onRemove} disabled={remove.isPending}>
            {remove.isPending ? 'Removing…' : 'Remove connection'}
          </Button>
        )}
        <Button type="button" variant="secondary" size="sm" onClick={onSave} disabled={save.isPending}>
          {save.isPending ? 'Saving…' : 'Save'}
        </Button>
      </div>
    </div>
  )
}

/**
 * Both connection cards plus the list query. The two cards share one cached
 * query, so a save in either refreshes both masks.
 */
export function ArrConnectionsPanel() {
  const connections = useArrConnections()

  // Be tolerant of a proxy/mocked body without `instances`. The cards render
  // while the list is still in flight so their fields and help are available
  // immediately; a freshly fetched connection is adopted once it arrives.
  const instances = Array.isArray(connections.data?.instances) ? connections.data.instances : []

  return (
    <div className="space-y-4">
      {connections.error && <Alert>{arrErrorMessage(connections.error)}</Alert>}
      <div className="grid gap-4 lg:grid-cols-2">
        {ARR_KINDS.map((kind) => (
          <ArrConnectionCard
            key={kind}
            kind={kind}
            connection={instances.find((candidate) => candidate.kind === kind)}
          />
        ))}
      </div>
    </div>
  )
}

/** Settings card for the shared section registry (nav + page title). */
export function ArrConnectionsSection() {
  const { title, description } = settingsSection('arr.connections')
  return (
    <Card id={settingsSectionId('arr.connections')} title={title} description={description}>
      <ArrConnectionsPanel />
    </Card>
  )
}
