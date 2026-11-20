import { useHealth } from '../lib/hooks'
import { Alert, Badge, Button, Card, PageHeader, Spinner } from '../components/ui'

function Flag({ label, value }: { label: string; value: boolean }) {
  return (
    <div className="flex items-center justify-between rounded-lg border border-ink-800 bg-ink-950/50 px-3 py-2">
      <span className="text-sm text-zinc-400">{label}</span>
      <Badge tone={value ? 'success' : 'warning'}>{value ? 'yes' : 'no'}</Badge>
    </div>
  )
}

export function SystemPage() {
  const health = useHealth()

  return (
    <div>
      <PageHeader
        title="System"
        description="Readiness of the appliance database, secrets and external tools."
        actions={
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void health.refetch()}
            disabled={health.isFetching}
          >
            {health.isFetching ? 'Refreshing…' : 'Refresh'}
          </Button>
        }
      />

      {health.isLoading && <Spinner label="Checking health" />}
      {health.error && <Alert>Could not load health: {health.error.message}</Alert>}

      {health.data && (
        <div className="space-y-6">
          <Card>
            <div className="flex flex-wrap items-center gap-3">
              <Badge tone={health.data.status === 'ok' ? 'success' : 'warning'}>
                {health.data.status}
              </Badge>
              <span className="text-sm text-zinc-400">
                version <span className="font-mono text-zinc-200">{health.data.version}</span>
              </span>
              <Badge tone={health.data.db_ok ? 'success' : 'danger'}>
                db {health.data.db_ok ? 'ok' : 'unreachable'}
              </Badge>
            </div>
            <div className="mt-4 grid gap-2 sm:grid-cols-3">
              <Flag label="Keyring" value={health.data.keyring_available} />
              <Flag label="Secrets initialised" value={health.data.secrets_initialized} />
              <Flag label="Auth initialised" value={health.data.auth_initialized} />
            </div>
          </Card>

          <div>
            <h2 className="mb-3 text-sm font-semibold uppercase tracking-wider text-zinc-500">
              External tools
            </h2>
            <div className="grid gap-3 sm:grid-cols-2">
              {Object.entries(health.data.tools).map(([name, tool]) => (
                <div
                  key={name}
                  className="rounded-xl border border-ink-800 bg-ink-900 p-4"
                  data-testid={`tool-${name}`}
                >
                  <div className="flex items-center justify-between gap-2">
                    <h3 className="font-mono text-sm text-zinc-100">{name}</h3>
                    <Badge tone={tool.available ? 'success' : 'warning'}>
                      {tool.available ? 'available' : 'not available'}
                    </Badge>
                  </div>
                  <p className="mt-2 text-xs text-zinc-500">
                    {tool.version ? `version ${tool.version}` : tool.available ? 'version unknown' : 'not built yet'}
                  </p>
                  {tool.path && (
                    <p className="mt-1 truncate font-mono text-[11px] text-zinc-600">{tool.path}</p>
                  )}
                </div>
              ))}
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
