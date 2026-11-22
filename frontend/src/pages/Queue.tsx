import type { QueueEligibility, QueueItem } from '../lib/api'
import {
  useApproveQueueItem,
  useQueue,
  useRejectQueueItem,
} from '../lib/hooks'
import { Alert, Badge, Button, Card, PageHeader, Spinner } from '../components/ui'

const eligibilityView: Record<
  QueueEligibility,
  { tone: 'success' | 'warning' | 'danger'; label: string }
> = {
  eligible: { tone: 'success', label: 'eligible' },
  waiting_for_tags: { tone: 'warning', label: 'waiting for tags' },
  missing_target: { tone: 'danger', label: 'missing target' },
}

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

function mediaHeading(item: QueueItem): string {
  if (item.title) return item.year ? `${item.title} (${item.year})` : item.title
  return item.filename
}

function episodeLabel(item: QueueItem): string | null {
  if (item.season_number == null || item.episode_number == null) return null
  const pad = (n: number) => String(n).padStart(2, '0')
  return `S${pad(item.season_number)}E${pad(item.episode_number)}`
}

function DonorLine({ item }: { item: QueueItem }) {
  if (item.donor_candidates.length === 0) {
    return (
      <p className="text-sm text-amber-300/90">
        No donor paired — drop a release matching the configured donor pattern next to the target.
      </p>
    )
  }
  return (
    <ul className="space-y-1">
      {item.donor_candidates.map((donor) => (
        <li key={donor.path} className="flex flex-wrap items-center gap-2 text-sm">
          <span className="truncate font-mono text-xs text-zinc-300" title={donor.path}>
            {donor.filename}
          </span>
          <span className="text-xs text-zinc-500">{formatBytes(donor.file_size)}</span>
          {donor.selected && <Badge tone="accent">pre-selected donor</Badge>}
        </li>
      ))}
    </ul>
  )
}

function QueueCard({
  item,
  onApprove,
  onReject,
  approving,
  rejecting,
}: {
  item: QueueItem
  onApprove: (id: number) => void
  onReject: (id: number) => void
  approving: boolean
  rejecting: boolean
}) {
  const view = eligibilityView[item.eligibility] ?? eligibilityView.missing_target
  const episode = episodeLabel(item)
  const busy = approving || rejecting

  return (
    <div data-testid={`queue-item-${item.id}`}>
      <Card className="overflow-hidden">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h2 className="truncate text-base font-semibold text-zinc-100" title={mediaHeading(item)}>
            {mediaHeading(item)}
          </h2>
          <p className="mt-1 text-xs text-zinc-500">
            donor file <span className="font-mono text-zinc-400">{item.filename}</span>
            {episode && <span className="ml-2 text-zinc-400">{episode}</span>}
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Badge tone={view.tone}>{view.label}</Badge>
          <Badge tone="neutral">{item.status}</Badge>
        </div>
      </div>

      <dl className="mt-4 grid gap-3 sm:grid-cols-2">
        <div className="sm:col-span-2">
          <dt className="text-[11px] uppercase tracking-wider text-zinc-500">Target</dt>
          <dd className="mt-1 truncate font-mono text-xs text-zinc-300" title={item.target_path}>
            {item.target_path || 'No matched target file'}
          </dd>
        </div>
        <div>
          <dt className="text-[11px] uppercase tracking-wider text-zinc-500">Audio languages</dt>
          <dd className="mt-1 flex flex-wrap gap-1">
            {item.audio_languages.length === 0 && (
              <span className="text-xs text-zinc-500">unknown</span>
            )}
            {item.audio_languages.map((lang) => (
              <Badge key={lang} tone="neutral">
                {lang}
              </Badge>
            ))}
            {item.original_language && (
              <Badge tone="accent">original {item.original_language}</Badge>
            )}
          </dd>
        </div>
        <div>
          <dt className="text-[11px] uppercase tracking-wider text-zinc-500">Donor pairing</dt>
          <dd className="mt-1">
            <DonorLine item={item} />
          </dd>
        </div>
      </dl>

      {item.eligibility === 'waiting_for_tags' && item.missing_tags.length > 0 && (
        <p className="mt-3 text-xs text-amber-300/80">
          Missing filename tags: {item.missing_tags.join(', ')}
        </p>
      )}

      <div className="mt-4 flex items-center justify-end gap-2 border-t border-ink-800 pt-4">
        <Button
          variant="secondary"
          size="sm"
          disabled={busy}
          onClick={() => onReject(item.id)}
        >
          {rejecting ? 'Rejecting…' : 'Reject'}
        </Button>
        <Button
          variant="primary"
          size="sm"
          disabled={busy}
          onClick={() => onApprove(item.id)}
        >
          {approving ? 'Approving…' : 'Approve'}
        </Button>
      </div>
    </Card>
    </div>
  )
}

export function QueuePage() {
  const queue = useQueue()
  const approve = useApproveQueueItem()
  const reject = useRejectQueueItem()
  const items = queue.data?.items ?? []
  const actionError = approve.error ?? reject.error

  return (
    <div>
      <PageHeader
        title="Approval Queue"
        description="Donor drops wait here until you approve them. Target, donor, languages and Tdarr Gate eligibility are pre-selected; no extraction happens in M2."
        actions={
          <>
            <Badge tone={items.length > 0 ? 'accent' : 'neutral'}>{items.length} pending</Badge>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => void queue.refetch()}
              disabled={queue.isFetching}
            >
              {queue.isFetching ? 'Refreshing…' : 'Refresh'}
            </Button>
          </>
        }
      />

      {queue.isLoading && <Spinner label="Loading approval queue" />}
      {queue.error && <Alert>Could not load the queue: {queue.error.message}</Alert>}
      {actionError && <Alert>Could not update the queue item: {actionError.message}</Alert>}

      {!queue.isLoading && !queue.error && items.length === 0 && (
        <Card>
          <div className="py-8 text-center">
            <p className="text-sm font-medium text-zinc-200">Queue is clear</p>
            <p className="mt-1 text-sm text-zinc-500">
              Drop a donor video into the watch folder; stable files appear here for approval.
            </p>
          </div>
        </Card>
      )}

      <div className="space-y-4">
        {items.map((item) => (
          <QueueCard
            key={item.id}
            item={item}
            onApprove={(id) => approve.mutate(id)}
            onReject={(id) => reject.mutate(id)}
            approving={approve.isPending && approve.variables === item.id}
            rejecting={reject.isPending && reject.variables === item.id}
          />
        ))}
      </div>
    </div>
  )
}
