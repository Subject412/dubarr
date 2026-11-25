import { useId, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import {
  ApiError,
  type LibrarySearchItem,
  type QueueDiagnostics,
  type QueueEligibility,
  type QueueIdentifyCandidate,
  type QueueIdentifyResponse,
  type QueueItem,
  type QueueMatchState,
} from '../lib/api'
import { LibraryRefreshError, refreshLibrary, relativeTime } from '../lib/library'
import { cn } from '../lib/cn'
import {
  queryKeys,
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

/**
 * The row's state, with the same `matched` fallback the badge uses: a payload
 * without `match_state` must not mislabel a matched row. `unmatched` is exactly
 * the `!matched` case, so the two never disagree.
 */
function matchStateOf(item: QueueItem): QueueMatchState {
  return item.match_state ?? (item.matched ? 'matched_no_donor' : 'unmatched')
}

/** Which precondition blocks a matched row that is not ready. */
type Blocker = 'donor' | 'donor_pattern' | 'gate_tag' | 'gate_excluded' | 'mount' | 'target' | 'neutral'

/**
 * Reads the blocker out of the server's `match_blocker`, the machine-readable
 * half of the same verdict `match_reason` words. The prose is never parsed:
 * rewording `match_reason` on the server must not silently change the hint the
 * operator gets.
 *
 * A missing value (older payload), `none`/`no_match` (cases the card handles
 * before asking for a hint) or a value this build does not know fall back to
 * the neutral hint instead of guessing a cause.
 */
function blockerOf(item: QueueItem): Blocker {
  switch (item.match_blocker) {
    case 'no_target':
      return 'target'
    case 'gate_tags':
      return 'gate_tag'
    case 'gate_excluded':
      return 'gate_excluded'
    case 'mount':
      return 'mount'
    case 'no_donor':
      // The payload proves whether a pattern exists at all: an empty list means
      // nothing was searched for, so "drop a donor next to the target" would
      // point at a folder that is never scanned. Older payloads omit the field,
      // and then neither cause is claimed.
      return item.donor_patterns && item.donor_patterns.length === 0 ? 'donor_pattern' : 'donor'
    default:
      return 'neutral'
  }
}

/** The next step for that blocker, pointing at the thing the operator can change. */
const blockerNextStep: Record<Blocker, string> = {
  donor: 'Drop a donor release matching the configured pattern next to the target in the watch folder, then refresh.',
  donor_pattern: 'Configure the donor filename pattern under Settings → Imports, then refresh.',
  gate_tag: 'Add the required filename tag to the target, then refresh.',
  gate_excluded: 'Remove the excluded filename tag from the target, then refresh.',
  mount: 'Check that the library mount is reachable, then refresh.',
  target: 'Sync the library once the target file exists, then refresh.',
  // No cause can be claimed: point at everything the card does show.
  neutral: 'Check the target, its Processing Gate verdict and the donor folder, then refresh.',
}

/**
 * One sentence of instruction per match state, plus the reason-bearing detail
 * for a matched-but-blocked row. The reason is reused verbatim so the page has
 * one vocabulary for "why not": the same prose as `match_reason`.
 */
function instructionFor(item: QueueItem): { state: QueueMatchState; text: string } {
  const state = matchStateOf(item)
  if (state === 'ready') return { state, text: 'Ready to approve.' }
  if (state === 'unmatched') {
    return { state, text: 'Not identified — identify this release, or match it manually.' }
  }
  const next = blockerNextStep[blockerOf(item)]
  // A payload without `match_reason` must not throw: the next step stands alone.
  const reason = (item.match_reason ?? '').trim()
  return { state, text: reason ? `${reason} ${next}` : next }
}

/**
 * Why Approve cannot be clicked, or null when it can. A row that is not `ready`
 * is never approvable — handleApprove answers 409 for exactly those cases (no
 * match, no target file, unreadable folder, no donor, an ineligible target) —
 * so the reason is `match_reason` rather than a second vocabulary.
 *
 * A local manual pick is the one exception, and only while the server still
 * reports the row unmatched: handleApprove accepts a client-supplied
 * library_cache_id for a row whose stored match is empty. Once a refetch reports
 * the row matched, its own verdict decides again — a pick that yields no donor
 * leaves `matched_no_donor`, which approve would refuse, so keeping the local
 * pick in force would only produce a 409.
 */
function approveBlockReason(item: QueueItem, manualMatch: LibrarySearchItem | null): string | null {
  if (manualMatch && !item.matched) return null
  if (matchStateOf(item) === 'ready') return null
  if (!item.matched) return 'Match this release to an arr library item first.'
  return (item.match_reason ?? '').trim() || 'This release is not ready to approve yet.'
}

/** Which empty state an empty queue is in, from the server's diagnostics. */
type EmptyQueueCase =
  | 'no_instance'
  | 'never_synced'
  | 'sync_failed'
  | 'mixed'
  | 'empty_library'
  | 'health_unavailable'
  | 'clear'

/**
 * Which empty state an empty queue is in.
 *
 * The server's counters are per INSTANCE, so they are only a statement about
 * the installation when they add up to it: `never_synced` needs every configured
 * instance never attempted, and `sync_failed` needs every attempted instance to
 * have failed — the same rule `library.ts` applies to its own empty states.
 * Anything in between is a countable partial fact (Radarr synced, Sonarr never
 * attempted), never "the library has not been synced yet" or "the last sync
 * failed" printed above a fresh `last_pass_at`.
 *
 * `sync_health_available === false` comes first: the counts are then zeroed and
 * prove nothing, least of all that no instance is configured.
 */
function emptyQueueCase(diagnostics: QueueDiagnostics | undefined): EmptyQueueCase {
  if (!diagnostics) return 'clear'
  if (diagnostics.sync_health_available === false) return 'health_unavailable'
  const configured = diagnostics.instances_configured
  if (configured <= 0) return 'no_instance'
  const neverSynced = diagnostics.never_synced
  const failed = diagnostics.sync_failed
  const attempted = configured - neverSynced
  if (neverSynced >= configured) return 'never_synced'
  // Only attempted instances can say anything about the sync: a never-attempted
  // instance must not veto a failure, and a success proves the pipeline works.
  if (attempted > 0 && failed >= attempted) return 'sync_failed'
  if (neverSynced > 0 || failed > 0) return 'mixed'
  // Every instance synced cleanly and the cache is still empty: the library
  // itself is empty, so the donor drop has nothing to be matched against.
  if (diagnostics.cache_items === 0) return 'empty_library'
  return 'clear'
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
  if (!item.matched) {
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
  const state = matchStateOf(item)
  const match = matchStateView[state]
  const instruction = instructionFor(item)
  const episode = episodeLabel(item)
  const busy = approving || rejecting
  const [manualMatch, setManualMatch] = useState<LibrarySearchItem | null>(null)
  const [identified, setIdentified] = useState<QueueIdentifyResponse | null>(null)
  const [identifyError, setIdentifyError] = useState<Error | null>(null)
  const [pickingId, setPickingId] = useState<number | null>(null)
  const [pickError, setPickError] = useState<Error | null>(null)
  const blockReason = approveBlockReason(item, manualMatch)
  // One id per card, so the disabled control describes exactly its own hint.
  const blockHintId = useId()

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
              screen-reader reachable. The ready row already says it in the
              instruction line below, so this would only repeat itself. */}
          {item.match_reason && state !== 'ready' && (
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
          {state === 'unmatched' && (
            <div className="mt-3 rounded-lg border border-amber-500/30 bg-amber-500/5 p-3">
              <p className="text-xs font-medium text-amber-300/90" data-testid="queue-instruction">
                {instruction.text}
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

      {/* One instruction per match state. `unmatched` already carries the
          identify panel (with the same sentence as its heading), so it needs
          nothing here. */}
      {state !== 'unmatched' && (
        <p
          data-testid="queue-instruction"
          className={cn(
            'mt-3 rounded-lg border p-3 text-xs',
            state === 'ready'
              ? 'border-emerald-500/30 bg-emerald-500/5 text-emerald-300/90'
              : 'border-amber-500/30 bg-amber-500/5 text-amber-300/90',
          )}
        >
          {instruction.text}
        </p>
      )}

      <div className="mt-4 flex flex-wrap items-center justify-end gap-2 border-t border-ink-800 pt-4">
        {blockReason && (
          // Visible prose, wired to the disabled control with aria-describedby:
          // a title tooltip neither fires on a disabled button nor reaches a
          // screen reader.
          <p
            id={blockHintId}
            data-testid="approve-blocked"
            className="mr-auto text-xs text-amber-300/90"
          >
            Approve unavailable: {blockReason}
          </p>
        )}
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
          disabled={busy || blockReason !== null}
          aria-describedby={blockReason ? blockHintId : undefined}
          onClick={() => onApprove(item.id, manualMatch?.id)}
        >
          {approving ? 'Approving…' : 'Approve'}
        </Button>
      </div>
    </Card>
    </div>
  )
}

/** Fixed copy per empty state; `mixed` is built from the counts, see below. */
const emptyQueueCopy: Record<Exclude<EmptyQueueCase, 'mixed'>, { headline: string; detail: string }> = {
  no_instance: {
    headline: 'No arr instance is configured yet.',
    detail:
      'Add a Radarr or Sonarr connection under Settings → Connections, then sync the library so donor drops can be matched against it.',
  },
  never_synced: {
    headline: 'The library has not been synced yet.',
    detail:
      'Dubarr has not polled Radarr or Sonarr, so there is no library to match donor drops against.',
  },
  sync_failed: {
    headline: 'The last sync failed.',
    detail:
      'Every attempted instance failed, so nothing can be matched until a pass succeeds. The Library page shows which instance failed and why.',
  },
  empty_library: {
    headline: 'The arr library is empty.',
    detail:
      'The last sync succeeded but Radarr or Sonarr returned no titles, so there is nothing to match a donor drop against. Check the connection and its root folders on the Library page.',
  },
  health_unavailable: {
    headline: 'Could not read sync health.',
    detail:
      'The library cache is unreadable right now, so this page cannot tell whether a sync is needed. Nothing here says how many instances are configured.',
  },
  clear: {
    headline: 'Queue is clear',
    detail:
      'Nothing is waiting for approval. Drop a donor video into the watch folder; stable files appear here for approval.',
  },
}

/**
 * A partly synced or partly failing installation is a countable fact, not a
 * verdict on the whole appliance: name the instances and what they did, and let
 * the healthy ones carry the library.
 */
function mixedEmptyCopy(diagnostics: QueueDiagnostics): { headline: string; detail: string } {
  const total = diagnostics.instances_configured
  const facts: string[] = []
  if (diagnostics.never_synced > 0) {
    facts.push(`${diagnostics.never_synced} of ${total} instances has not synced yet`)
  }
  if (diagnostics.sync_failed > 0) {
    facts.push(`${diagnostics.sync_failed} of ${total} instances failed its last sync`)
  }
  return {
    headline: 'Some arr instances are not synced.',
    detail: `${facts.join(', and ')}; the rest are healthy, so donor drops are matched against what they already cached. The Library page shows which instance is which.`,
  }
}

function emptyQueueCopyFor(
  kind: EmptyQueueCase,
  diagnostics: QueueDiagnostics | undefined,
): { headline: string; detail: string } {
  if (kind === 'mixed' && diagnostics) return mixedEmptyCopy(diagnostics)
  return emptyQueueCopy[kind === 'mixed' ? 'clear' : kind]
}

/**
 * An empty list is not always "nothing to do" — an appliance with no arr
 * connection, no first sync or a failing sync looks exactly the same. The
 * server's diagnostics decide which one this is, and each case names the next
 * step instead of inviting a pointless retry.
 */
function EmptyQueue({
  diagnostics,
  onSynced,
}: {
  diagnostics: QueueDiagnostics | undefined
  onSynced: () => void
}) {
  const kind = emptyQueueCase(diagnostics)
  const copy = emptyQueueCopyFor(kind, diagnostics)
  const [syncing, setSyncing] = useState(false)
  const [note, setNote] = useState<string | null>(null)

  // Reuses the Library page's refresh call: same endpoint, same 409 codes, so
  // the queue never grows a second sync path.
  const startSync = () => {
    if (syncing) return
    setSyncing(true)
    setNote(null)
    refreshLibrary('')
      .then(() => {
        onSynced()
        setNote('Sync started — refresh once the pass finishes.')
      })
      .catch((error: unknown) => {
        if (error instanceof LibraryRefreshError) {
          setNote(
            error.code === 'busy'
              ? 'A sync pass is already running.'
              : `Could not start a sync: ${error.message}`,
          )
          return
        }
        setNote(`Could not start a sync: ${error instanceof Error ? error.message : 'unknown error'}`)
      })
      .finally(() => setSyncing(false))
  }

  return (
    <Card>
      <div className="py-8 text-center" data-testid="queue-empty" data-empty-case={kind}>
        <p className="text-sm font-medium text-zinc-200">{copy.headline}</p>
        <p className="mt-1 text-sm text-zinc-500">{copy.detail}</p>
        {kind === 'sync_failed' && diagnostics?.last_pass_at && (
          <p className="mt-1 text-xs text-zinc-500">
            Last successful sync: {relativeTime(diagnostics.last_pass_at)}.
          </p>
        )}
        {kind === 'no_instance' && (
          <p className="mt-3">
            <Link
              to="/settings?section=arr.connections"
              className="text-sm text-accent-400 underline underline-offset-2 hover:text-accent-300"
            >
              Add a connection in Settings
            </Link>
          </p>
        )}
        {kind === 'never_synced' && (
          <div className="mt-3 flex justify-center">
            <Button variant="secondary" size="sm" disabled={syncing} onClick={startSync}>
              {syncing ? 'Syncing…' : 'Refresh now'}
            </Button>
          </div>
        )}
        {kind === 'mixed' && (
          <div className="mt-3 flex justify-center">
            <Button variant="secondary" size="sm" disabled={syncing} onClick={startSync}>
              {syncing ? 'Syncing…' : 'Sync the remaining instances'}
            </Button>
          </div>
        )}
        {(kind === 'sync_failed' || kind === 'empty_library' || kind === 'health_unavailable') && (
          <p className="mt-3">
            <Link
              to="/library"
              className="text-sm text-accent-400 underline underline-offset-2 hover:text-accent-300"
            >
              {kind === 'sync_failed'
                ? 'Check the sync health on the Library page'
                : kind === 'empty_library'
                  ? 'Open the Library page'
                  : 'Check the Library page'}
            </Link>
          </p>
        )}
        {note && (
          <p className="mt-2 text-xs text-amber-300/90" data-testid="queue-empty-note">
            {note}
          </p>
        )}
      </div>
    </Card>
  )
}

export function QueuePage() {
  const queue = useQueue()
  const approve = useApproveQueueItem()
  const reject = useRejectQueueItem()
  const identify = useIdentifyQueueItem()
  const match = useMatchQueueItem()
  const client = useQueryClient()
  const items = queue.data?.items ?? []
  const actionError = approve.error ?? reject.error

  return (
    <div>
      <PageHeader
        title="Approval Queue"
        description="Donor drops wait here until you approve them. Target, donor, languages and Processing Gate eligibility are pre-selected, and every card says what is still blocking it and what to do next."
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
        <EmptyQueue
          diagnostics={queue.data?.diagnostics}
          onSynced={() => void client.invalidateQueries({ queryKey: queryKeys.queue })}
        />
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
