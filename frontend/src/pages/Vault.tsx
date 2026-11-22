import type { VaultItem } from '../lib/api'
import { useVault } from '../lib/hooks'
import { Alert, Badge, Button, Card, PageHeader, Spinner } from '../components/ui'

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  const units = ['KiB', 'MiB', 'GiB', 'TiB']
  let value = bytes / 1024
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit += 1
  }
  return `${value.toFixed(1)} ${units[unit]}`
}

function mediaHeading(item: VaultItem): string {
  if (item.season_number != null && item.episode_number != null) {
    const pad = (n: number) => String(n).padStart(2, '0')
    return `S${pad(item.season_number)}E${pad(item.episode_number)}`
  }
  return item.media_type === 'movie' ? 'Movie' : 'Episode'
}

function createdLabel(created: string): string {
  return created.replace('T', ' ').replace(/\.\d+Z?$/, '')
}

function VaultRow({ item }: { item: VaultItem }) {
  return (
    <tr
      data-testid={`vault-item-${item.id}`}
      className="border-b border-ink-800/70 last:border-0 hover:bg-ink-800/40"
    >
      <td className="max-w-[22rem] px-4 py-3 align-top">
        <p className="truncate font-mono text-xs text-zinc-200" title={item.filename}>
          {item.filename}
        </p>
        {item.source_release_name && (
          <p className="mt-1 truncate text-[11px] text-zinc-500" title={item.source_release_name}>
            donor {item.source_release_name}
          </p>
        )}
      </td>
      <td className="px-4 py-3 align-top">
        <p className="truncate text-sm text-zinc-200" title={item.title}>
          {item.title}
        </p>
        <p className="mt-0.5 text-[11px] uppercase tracking-wider text-zinc-500">
          {mediaHeading(item)}
        </p>
      </td>
      <td className="px-4 py-3 align-top">
        <div className="flex flex-wrap gap-1">
          <Badge tone="accent">{item.language}</Badge>
          {item.variant !== 'standard' && <Badge tone="neutral">{item.variant}</Badge>}
        </div>
      </td>
      <td className="px-4 py-3 align-top">
        <p className="text-sm text-zinc-200">
          <span>{item.codec}</span> <span className="text-zinc-500">·</span>{' '}
          <span>{item.channels}</span>
        </p>
        <p className="mt-0.5 text-[11px] text-zinc-500">{item.channel_count} ch</p>
      </td>
      <td className="whitespace-nowrap px-4 py-3 align-top text-sm text-zinc-300">
        {formatBytes(item.file_size)}
      </td>
      <td className="max-w-[14rem] px-4 py-3 align-top">
        <span
          className="block truncate font-mono text-[11px] text-zinc-400"
          title={item.sha256}
        >
          {item.sha256}
        </span>
      </td>
      <td className="whitespace-nowrap px-4 py-3 align-top text-xs text-zinc-500">
        {createdLabel(item.created_at)}
      </td>
    </tr>
  )
}

export function VaultPage() {
  const vault = useVault()
  const items = vault.data?.items ?? []

  return (
    <div>
      <PageHeader
        title="Audio Vault"
        description="Immutable .mka tracks archived from approved donor releases. Read-only: every track is written once and verified by SHA-256."
        actions={
          <>
            <Badge tone={items.length > 0 ? 'accent' : 'neutral'}>{items.length} tracks</Badge>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => void vault.refetch()}
              disabled={vault.isFetching}
            >
              {vault.isFetching ? 'Refreshing…' : 'Refresh'}
            </Button>
          </>
        }
      />

      {vault.isLoading && <Spinner label="Loading vault" />}
      {vault.error && <Alert>Could not load the vault: {vault.error.message}</Alert>}

      {!vault.isLoading && !vault.error && items.length === 0 && (
        <Card>
          <div className="py-8 text-center">
            <p className="text-sm font-medium text-zinc-200">Vault is empty</p>
            <p className="mx-auto mt-1 max-w-md text-sm text-zinc-500">
              The vault fills as approvals complete: approved donors are extracted, verified by
              SHA-256 and archived here as write-once tracks.
            </p>
          </div>
        </Card>
      )}

      {items.length > 0 && (
        <section className="overflow-hidden rounded-xl border border-ink-800 bg-ink-900 shadow-sm">
          <div className="overflow-x-auto">
            <table className="w-full min-w-[60rem] border-collapse text-left">
              <thead>
                <tr className="border-b border-ink-800 bg-ink-900/60 text-[11px] uppercase tracking-wider text-zinc-500">
                  <th scope="col" className="px-4 py-3 font-medium">File</th>
                  <th scope="col" className="px-4 py-3 font-medium">Media</th>
                  <th scope="col" className="px-4 py-3 font-medium">Language</th>
                  <th scope="col" className="px-4 py-3 font-medium">Audio</th>
                  <th scope="col" className="px-4 py-3 font-medium">Size</th>
                  <th scope="col" className="px-4 py-3 font-medium">SHA-256</th>
                  <th scope="col" className="px-4 py-3 font-medium">Added</th>
                </tr>
              </thead>
              <tbody>
                {items.map((item) => (
                  <VaultRow key={item.id} item={item} />
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}
    </div>
  )
}
