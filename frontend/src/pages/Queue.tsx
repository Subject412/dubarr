import { useState } from 'react'
import {
  ApiError,
  type LibrarySearchItem,
  type QueueEligibility,
  type QueueIdentifyCandidate,
  type QueueIdentifyResponse,
  type QueueItem,
  type QueueMatchState,
} from '../lib/api'
import {
  useApproveQueueItem,
  useDebouncedValue,
  useIdentifyQueueItem,
  useLibrarySearch,
  useMatchQueueItem,
  useQueue,
  useRejectQueueItem,
} from '../lib/hooks'
import { Alert, Badge, Button, Card, Input, PageHeader, Spinner } from '../components/ui'

const eligibilityView: Record<
  QueueEligibility,
  { tone: 'success' | 'warning' | 'danger'; label: string }
> = {
  eligible: { tone: 'success', label: 'eligible' },
  waiting_for_tags: { tone: 'warning', label: 'waiting for tags' },
  missing_target: { tone: 'danger', label: 'missing target' },
}

/** Distinct treatment per match state so a matched-but-blocked row never looks approvable. */
const matchStateView: Record<
  QueueMatchState,
  { tone: 'success' | 'warning' | 'danger'; label: string }
> = {
  unmatched: { tone: 'danger', label: 'Not matched' },
  matched_no_donor: { tone: 'warning', label: 'Matched, not ready' },
  ready: { tone: 'success', label: 'Ready' },
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

function libraryLabel(entry: LibrarySearchItem): string {
  const title = entry.year ? `${entry.title} (${entry.year})` : entry.title
  if (entry.season_number == null || entry.episode_number == null) return title
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${title} S${pad(entry.season_number)}E${pad(entry.episode_number)}`
}

/** Inline typeahead over the arr-synced library for the manual match fallback. */
function LibrarySearchBox({
  onSelect,
}: {
  onSelect: (entry: LibrarySearchItem) => void
}) {
  const [query, setQuery] = useState('')
  const debounced = useDebouncedValue(query, 300)
  const search = useLibrarySearch(debounced)
  const tooShort = debounced.trim().length < 2

  return (
    <div className="mt-2">
      <Input
        value={query}
        onChange={(event) => setQuery(event.target.value)}
        placeholder="Search the arr library…"
        aria-label="Search the arr library"
        autoComplete="off"
      />
      {search.isFetching && <p className="mt-1 text-xs text-zinc-500">Searching…</p>}
      {search.error && (
        <p className="mt-1 text-xs text-red-400">Library search failed: {search.error.message}</p>
      )}
      {!tooShort && !search.isFetching && search.data && search.data.items.length === 0 && (
        <p className="mt-1 text-xs text-zinc-500">No library matches.</p>
      )}
      {!tooShort && search.data && search.data.items.length > 0 && (
        <ul className="mt-2 max-h-48 space-y-1 overflow-y-auto">
          {search.data.items.map((entry) => (
            <li key={entry.id}>
              <button
                type="button"
                onClick={() => onSelect(entry)}
                className="flex w-full items-center justify-between gap-2 rounded-md border border-ink-700 bg-ink-950/60 px-3 py-1.5 text-left text-sm text-zinc-200 hover:border-accent-500 hover:bg-ink-800"
              >
                <span className="truncate">{libraryLabel(entry)}</span>
                <span className="shrink-0 text-xs text-zinc-500">
                  {entry.media_type}
                  {entry.original_language_iso2 ? ` · ${entry.original_language_iso2}` : ''}
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function DonorLine({ item }: { item: QueueItem }) {
  // No arr target means there is no folder to drop a sibling release next to,
  // so the drop guidance would point at nothing.
  if (item.match_state === 'unmatched') {
    return <p className="text-sm text-zinc-500">No target to pair a donor with.</p>
  }
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

/** Explains an identify failure in the operator's terms, not arr's. */
function IdentifyFailure({ error }: { error: Error }) {
  const notConfigured = error instanceof ApiError && error.code === 'not_configured'
  if (notConfigured) {
    return (
      <p className="mt-2 text-xs text-amber-300/90">
        arr is not configured for this release — add the instance under Settings → Connections.{' '}
        <span className="text-zinc-500">({error.message})</span>
      </p>
    )
  }
  return <p className="mt-2 text-xs text-red-400">Identify failed: {error.message}</p>
}

/**
 * The identify result: what arr thought of the release name and the library rows
 * it matched. The server never pre-picks — the operator does.
 */
function IdentifyResult({
  result,
  pickingId,
  onPick,
  pickError,
}: {
  result: QueueIdentifyResponse
  pickingId: number | null
  onPick: (candidate: QueueIdentifyCandidate) => void
  pickError: Error | null
}) {
  return (
    <div className="mt-2" data-testid="identify-result">
      {result.parse.parsed ? (
        <p className="text-xs text-zinc-400">
          arr parsed{' '}
          <span className="font-medium text-zinc-200">
            {result.parse.title}
            {result.parse.year ? ` (${result.parse.year})` : ''}
          </span>
          {result.parse.quality ? ` · ${result.parse.quality}` : ''}
          {result.parse.confidence ? ` · ${Math.round(result.parse.confidence * 100)}%` : ''}
        </p>
      ) : (
        // An unparsable release is not an empty library: say which one it is.
        <p className="text-xs text-amber-300/90" data-testid="identify-unparsable">
          arr could not parse this release name. Enter a title and try again.
        </p>
      )}
      {!result.parse.parsed ? null : result.candidates.length === 0 ? (
        <p className="mt-1 text-xs text-zinc-500">
          No library candidates — sync the arr library, or search it manually below.
        </p>
      ) : (
        <ul className="mt-2 space-y-1">
          {result.candidates.map((candidate) => (
            <li key={candidate.library_cache_id}>
              <button
                type="button"
                onClick={() => onPick(candidate)}
                disabled={pickingId !== null}
                className="flex w-full items-center justify-between gap-2 rounded-md border border-ink-700 bg-ink-950/60 px-3 py-1.5 text-left text-sm text-zinc-200 hover:border-accent-500 hover:bg-ink-800 disabled:opacity-60"
              >
                <span className="truncate">
                  {candidate.year ? `${candidate.title} (${candidate.year})` : candidate.title}
                </span>
                <span className="shrink-0 text-xs text-zinc-500">
                  {pickingId === candidate.library_cache_id
                    ? 'matching…'
                    : `${Math.round(candidate.confidence * 100)}% · ${candidate.media_type}`}
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
      {pickError && <p className="mt-1 text-xs text-red-400">Could not match: {pickError.message}</p>}
    </div>
  )
}

function QueueCard({
  item,
  onApprove,
  onReject,
  onIdentify,
  onMatch,
  approving,
  rejecting,
  identifying,
}: {
  item: QueueItem
  onApprove: (id: number, libraryCacheId?: number) => void
  onReject: (id: number) => void
  onIdentify: (id: number) => Promise<QueueIdentifyResponse>
  onMatch: (id: number, candidate: QueueIdentifyCandidate) => Promise<unknown>
  approving: boolean
  rejecting: boolean
  identifying: boolean
}) {
  const view = eligibilityView[item.eligibility] ?? eligibilityView.missing_target
  // A missing or unexpected match_state must not mislabel a matched row, so the
  // fallback is derived from `matched` — the field the backend invariant ties
  // `unmatched` to.
  const match =
    matchStateView[item.match_state] ??
    (item.matched ? matchStateView.matched_no_donor : matchStateView.unmatched)
  const episode = episodeLabel(item)
  const busy = approving || rejecting
  const [manualMatch, setManualMatch] = useState<LibrarySearchItem | null>(null)
  const [identified, setIdentified] = useState<QueueIdentifyResponse | null>(null)
  const [identifyError, setIdentifyError] = useState<Error | null>(null)
  const [pickingId, setPickingId] = useState<number | null>(null)
  const [pickError, setPickError] = useState<Error | null>(null)
  const needsMatch = !item.matched && manualMatch === null

  const runIdentify = () => {
    setIdentifyError(null)
    setPickError(null)
    onIdentify(item.id)
      .then(setIdentified)
      .catch(setIdentifyError)
  }

  const pickCandidate = (candidate: QueueIdentifyCandidate) => {
    setPickError(null)
    setPickingId(candidate.library_cache_id)
    onMatch(item.id, candidate)
      .catch(setPickError)
      .finally(() => setPickingId(null))
  }

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
        <div className="flex flex-wrap items-center justify-end gap-2">
          <Badge tone={match.tone}>{match.label}</Badge>
          <Badge tone={view.tone}>{view.label}</Badge>
          <Badge tone="neutral">{item.status}</Badge>
          {/* Visible prose, not a title: a tooltip is neither keyboard nor
              screen-reader reachable. */}
          {item.match_reason && (
            <p className="basis-full text-right text-xs text-zinc-500">{item.match_reason}</p>
          )}
        </div>
      </div>

      <dl className="mt-4 grid gap-3 sm:grid-cols-2">
        <div className="sm:col-span-2">
          <dt className="text-[11px] uppercase tracking-wider text-zinc-500">Target</dt>
          <dd className="mt-1 truncate font-mono text-xs text-zinc-300" title={item.target_path}>
            {item.target_path || 'No matched target file'}
          </dd>
          {!item.matched && (
            <div className="mt-3 rounded-lg border border-amber-500/30 bg-amber-500/5 p-3">
              <p className="text-xs font-medium text-amber-300/90">
                No arr library match — identify this release to approve it.
              </p>
              <div className="mt-2 flex flex-wrap items-center gap-2">
                <Button variant="secondary" size="sm" disabled={identifying} onClick={runIdentify}>
                  {identifying ? 'Identifying…' : 'Identify with arr'}
                </Button>
              </div>
              {identifyError && <IdentifyFailure error={identifyError} />}
              {identified && (
                <IdentifyResult
                  result={identified}
                  pickingId={pickingId}
                  onPick={pickCandidate}
                  pickError={pickError}
                />
              )}
              {manualMatch ? (
                <div className="mt-2 flex flex-wrap items-center justify-between gap-2">
                  <span className="text-sm text-zinc-200">
                    Matched to {libraryLabel(manualMatch)}
                  </span>
                  <Button variant="ghost" size="sm" onClick={() => setManualMatch(null)}>
                    Change
                  </Button>
                </div>
              ) : (
                <LibrarySearchBox onSelect={setManualMatch} />
              )}
            </div>
          )}
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
          disabled={busy || needsMatch}
          title={needsMatch ? 'Match this release to an arr library item first' : undefined}
          onClick={() => onApprove(item.id, manualMatch?.id)}
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
  const identify = useIdentifyQueueItem()
  const match = useMatchQueueItem()
  const items = queue.data?.items ?? []
  const actionError = approve.error ?? reject.error

  return (
    <div>
      <PageHeader
        title="Approval Queue"
        description="Donor drops wait here until you approve them. Target, donor, languages and Processing Gate eligibility are pre-selected; no extraction happens in M2."
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
            onApprove={(id, libraryCacheId) => approve.mutate({ id, libraryCacheId })}
            onReject={(id) => reject.mutate(id)}
            onIdentify={(id) => identify.mutateAsync(id)}
            onMatch={(id, candidate) =>
              match.mutateAsync({
                id,
                libraryCacheId: candidate.library_cache_id,
                confidence: candidate.confidence,
              })
            }
            approving={approve.isPending && approve.variables?.id === item.id}
            rejecting={reject.isPending && reject.variables === item.id}
            identifying={identify.isPending && identify.variables === item.id}
          />
        ))}
      </div>
    </div>
  )
}
