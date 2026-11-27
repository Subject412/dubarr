import { useState } from 'react'
import { ApiError, type VaultItem, type VaultPlanFile, type VaultPlanResponse } from '../lib/api'
import { useOrganiseApply, useOrganisePreview, useOrganiseRuns, useVault } from '../lib/hooks'
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

/** "1 file" / "41 files" — the count is always read out with its noun. */
function countFiles(count: number): string {
  return `${count} ${count === 1 ? 'file' : 'files'}`
}

function countRenames(count: number): string {
  return `${count} ${count === 1 ? 'rename' : 'renames'}`
}

/**
 * The plan's conflicts in words. `kind` is a machine name (`PlanConflict.Kind`),
 * so the UI never parses the server's sentence; an unknown kind still gets a
 * plain explanation rather than a raw identifier.
 */
const CONFLICT_TEXT: Record<string, { plan: string; row: string }> = {
  destination_duplicate: {
    plan: 'Two tracks would end up with the same file name.',
    row: 'Another track would get this same name.',
  },
  destination_exists: {
    plan: 'A file with this name is already in the vault.',
    row: 'This name is already taken.',
  },
  source_missing: {
    plan: "A track's file is missing from the vault.",
    row: 'This file is missing from the vault.',
  },
  destination_parent: {
    plan: 'A destination folder cannot be used.',
    row: 'This folder cannot be used.',
  },
}

/** What one plan row would do, in the operator's words. */
function rowOutcome(file: VaultPlanFile): {
  label: string
  tone: 'accent' | 'neutral' | 'danger'
  detail: string
} {
  if (file.conflict) {
    return {
      label: 'cannot move',
      tone: 'danger',
      detail: CONFLICT_TEXT[file.conflict]?.row ?? 'This name cannot be used.',
    }
  }
  if (!file.changed) {
    return { label: 'will not move', tone: 'neutral', detail: 'Already matches the pattern.' }
  }
  return { label: 'will move', tone: 'accent', detail: 'The name comes from your pattern.' }
}

/** The deepest folder every destination shares — the confirmation's "inside …". */
function commonRoot(paths: string[]): string {
  const split = paths
    .filter((path) => path !== '')
    .map((path) => path.split('/'))
    .filter((parts) => parts.length > 1)
  if (split.length === 0) return '/'
  const first = split[0]
  let end = first.length - 1
  for (const parts of split.slice(1)) {
    let index = 0
    while (index < end && index < parts.length - 1 && parts[index] === first[index]) index += 1
    end = Math.min(end, index)
  }
  return first.slice(0, end).join('/') || '/'
}

/** A refusal, already translated into what the operator has to do next. */
type Failure = {
  message: string
  /** Paths the server asks a human to inspect (manual_attention, apply_failed). */
  paths: string[]
  /** True when the library was reconciled to disk and needs eyes on it. */
  needsAttention: boolean
}

function failurePaths(payload: unknown): string[] {
  if (!payload || typeof payload !== 'object') return []
  const files = (payload as { files?: unknown }).files
  if (!Array.isArray(files)) return []
  return files.filter((file): file is string => typeof file === 'string' && file !== '')
}

/**
 * Translate an organise refusal. The refusal codes are the contract; the
 * server's prose is only a fallback for a code this build does not know.
 */
function describeFailure(error: unknown): Failure {
  const code = error instanceof ApiError ? error.code : undefined
  const payload = error instanceof ApiError ? error.payload : undefined
  const paths = failurePaths(payload)
  const message = error instanceof Error ? error.message : 'Something went wrong.'

  switch (code) {
    case 'stale_plan':
      return {
        message: 'The vault changed since the preview. Preview again to see the names it would use now.',
        paths,
        needsAttention: false,
      }
    case 'jobs_running':
      return {
        message:
          'A sync job is muxing right now. Wait for it to finish, then preview again — nothing was changed.',
        paths,
        needsAttention: false,
      }
    case 'conflict':
      return {
        message: 'Two files would get the same name, or a name is already taken. Nothing was changed.',
        paths,
        needsAttention: false,
      }
    case 'path_cycle':
      return {
        message: 'The new names would swap two files. Nothing was changed — adjust the pattern.',
        paths,
        needsAttention: false,
      }
    case 'cross_device':
      return {
        message:
          'The destination is on a different disk, so the files cannot be moved in one step. Nothing was changed.',
        paths,
        needsAttention: false,
      }
    case 'path_escape':
      return {
        message: 'A destination would land outside the Audio Vault. Nothing was changed.',
        paths,
        needsAttention: false,
      }
    case 'invalid_template':
    case 'name_invalid':
      return {
        message: `The naming pattern cannot produce these names. Fix it in Settings → Vault naming, then preview again.${
          message ? ` (${message})` : ''
        }`,
        paths,
        needsAttention: false,
      }
    case 'plan_too_large':
      return {
        message: 'This vault has too many files for one plan. Preview one media type at a time.',
        paths,
        needsAttention: false,
      }
    case 'vault_not_configured':
      return {
        message: 'The Audio Vault folder is not set up yet. Set it in Settings → Storage paths.',
        paths,
        needsAttention: false,
      }
    case 'apply_failed': {
      const stage = (payload as { stage?: unknown } | undefined)?.stage
      const text =
        stage === 'sql'
          ? 'Recording the new paths failed, so every file was put back where it was. Nothing was changed.'
          : stage === 'rename'
            ? 'Moving the files failed and every move made before it was undone. Nothing was changed.'
            : 'A destination folder could not be created, so nothing was changed.'
      return { message: text, paths, needsAttention: false }
    }
    case 'manual_attention':
      return {
        message:
          'The rename partly failed and could not be undone. The library now lists whatever is on disk. Check these paths before doing anything else:',
        paths,
        needsAttention: true,
      }
    default:
      return { message, paths, needsAttention: false }
  }
}

const RUN_STATUS: Record<string, { label: string; tone: 'success' | 'warning' | 'danger' }> = {
  applied: { label: 'Files moved', tone: 'success' },
  rolled_back: { label: 'Put back — nothing changed', tone: 'warning' },
  needs_attention: { label: 'Needs your attention', tone: 'danger' },
}

export function VaultPage() {
  const vault = useVault()
  const runs = useOrganiseRuns()
  const preview = useOrganisePreview()
  const apply = useOrganiseApply()

  const [plan, setPlan] = useState<VaultPlanResponse | null>(null)
  const [previewFailure, setPreviewFailure] = useState<Failure | null>(null)
  const [applyFailure, setApplyFailure] = useState<Failure | null>(null)
  const [confirming, setConfirming] = useState(false)
  const [done, setDone] = useState('')

  const items = vault.data?.items ?? []
  const runRows = runs.data?.items ?? []

  const runPreview = () => {
    setDone('')
    setApplyFailure(null)
    setConfirming(false)
    preview.mutate(
      { media_type: 'all' },
      {
        onSuccess: (data) => {
          setPreviewFailure(null)
          setPlan(data)
        },
        onError: (error) => {
          // A preview that could not be planned has no rows to show.
          setPlan(null)
          setPreviewFailure(describeFailure(error))
        },
      },
    )
  }

  const planId = plan?.plan_id
  const blockedReason = !plan
    ? 'Preview first — nothing here is a plan yet.'
    : plan.conflicts.length > 0
      ? 'Fix the conflicts above first. Nothing can be applied while a name is unusable.'
      : planId === undefined
        ? // No plan_id means the server will not accept this plan: preview again.
          'This preview cannot be applied. Press Preview changes again.'
        : plan.impact.renamed === 0
          ? 'Every file already matches the pattern, so there is nothing to move.'
          : null

  const applyPlan = () => {
    if (!planId) return
    apply.mutate(planId, {
      onSuccess: (result) => {
        setPlan(null)
        setConfirming(false)
        setApplyFailure(null)
        const unchanged =
          result.unchanged > 0 ? ` ${countFiles(result.unchanged)} already matched the pattern.` : ''
        setDone(`Done: ${countFiles(result.renamed)} moved.${unchanged}`)
      },
      onError: (error) => {
        setApplyFailure(describeFailure(error))
        setConfirming(false)
        // A muxing job keeps the preview valid: nothing moved and the same
        // plan can be applied once the job is done. Every other refusal means
        // the plan no longer describes the vault, so it must be re-previewed.
        if (!(error instanceof ApiError) || error.code !== 'jobs_running') setPlan(null)
      },
    })
  }

  const root = plan ? commonRoot(plan.files.map((file) => file.destination)) : ''

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

      <Card
        title="Reorganise the vault"
        description="Preview the names your patterns in Settings → Vault naming would give every track, then apply the whole plan at once. A preview writes nothing; an apply either moves every file or none."
        actions={
          <Button size="sm" onClick={runPreview} disabled={preview.isPending}>
            {preview.isPending ? 'Previewing…' : 'Preview changes'}
          </Button>
        }
      >
        <div className="space-y-4">
          {previewFailure && <Alert>{previewFailure.message}</Alert>}
          {done && <Alert tone="success">{done}</Alert>}

          {applyFailure && (
            <div data-testid="organise-attention">
              <Alert tone={applyFailure.needsAttention ? 'danger' : 'warning'}>
                <p>{applyFailure.message}</p>
                {applyFailure.paths.length > 0 && (
                  <ul className="mt-2 space-y-1 font-mono text-xs">
                    {applyFailure.paths.map((path) => (
                      <li key={path} className="break-all">
                        {path}
                      </li>
                    ))}
                  </ul>
                )}
              </Alert>
            </div>
          )}

          {!plan && !previewFailure && (
            <p className="text-sm text-zinc-500">
              Press Preview changes to see, file by file, what would move. Nothing is touched until
              you apply that plan.
            </p>
          )}

          {plan && (
            <>
              <div className="space-y-1 text-sm text-zinc-300">
                <p>
                  {countFiles(plan.impact.renamed)} will move and {countFiles(plan.impact.unchanged)}{' '}
                  will stay where they are.
                </p>
                {plan.impact.jobs_referencing_tracks > 0 && (
                  <p className="text-xs text-zinc-500">
                    {plan.impact.jobs_referencing_tracks}{' '}
                    {plan.impact.jobs_referencing_tracks === 1
                      ? 'sync job refers'
                      : 'sync jobs refer'}{' '}
                    to these tracks by id, so they keep working after the move.
                  </p>
                )}
              </div>

              {plan.impact.jobs_blocked > 0 && (
                <Alert tone="warning">
                  A sync job is analysing or muxing right now, so the plan cannot be applied yet.
                  Nothing was changed.
                </Alert>
              )}

              {plan.conflicts.length > 0 && (
                <div
                  data-testid="organise-conflicts"
                  className="rounded-lg border border-red-800/70 bg-red-950/30 px-3 py-2"
                >
                  <p className="text-sm font-medium text-red-200">
                    These must be fixed first — nothing can be applied yet.
                  </p>
                  <ul className="mt-2 space-y-2">
                    {plan.conflicts.map((conflict) => (
                      <li key={`${conflict.kind}-${conflict.destination}`}>
                        <p className="text-sm text-red-200">
                          {CONFLICT_TEXT[conflict.kind]?.plan ?? 'This name cannot be used.'}
                        </p>
                        <p className="break-all font-mono text-xs text-zinc-300">
                          {conflict.destination}
                        </p>
                        <p className="text-xs text-zinc-400">
                          Tracks {conflict.track_ids.join(', ')} — adjust the pattern in Settings →
                          Vault naming, then preview again.
                        </p>
                      </li>
                    ))}
                  </ul>
                </div>
              )}

              {plan.files.length > 0 && (
                <div className="overflow-x-auto rounded-lg border border-ink-800">
                  <table aria-label="Planned renames" className="w-full min-w-[48rem] border-collapse text-left">
                    <thead>
                      <tr className="border-b border-ink-800 bg-ink-900/60 text-[11px] uppercase tracking-wider text-zinc-500">
                        <th scope="col" className="px-4 py-3 font-medium">Now</th>
                        <th scope="col" className="px-4 py-3 font-medium">After</th>
                        <th scope="col" className="px-4 py-3 font-medium">What happens</th>
                      </tr>
                    </thead>
                    <tbody>
                      {plan.files.map((file) => {
                        const outcome = rowOutcome(file)
                        return (
                          <tr
                            key={file.track_id}
                            data-testid={`plan-row-${file.track_id}`}
                            className="border-b border-ink-800/70 last:border-0"
                          >
                            <td className="max-w-[18rem] px-4 py-3 align-top">
                              <p className="break-all font-mono text-xs text-zinc-200">
                                {file.source}
                              </p>
                            </td>
                            <td className="max-w-[22rem] px-4 py-3 align-top">
                              <p className="break-all font-mono text-xs text-zinc-200">
                                {file.destination}
                              </p>
                            </td>
                            <td className="px-4 py-3 align-top">
                              <Badge tone={outcome.tone}>{outcome.label}</Badge>
                              <p className="mt-1 text-xs text-zinc-500">{outcome.detail}</p>
                            </td>
                          </tr>
                        )
                      })}
                    </tbody>
                  </table>
                </div>
              )}

              {plan.files.length === 0 && (
                <p className="text-sm text-zinc-400">
                  The vault has no archived tracks yet, so there is nothing to rename.
                </p>
              )}
            </>
          )}

          <div className="flex flex-wrap items-center gap-3 border-t border-ink-800 pt-4">
            <Button onClick={() => setConfirming(true)} disabled={Boolean(blockedReason) || apply.isPending}>
              {apply.isPending ? 'Moving files…' : plan ? `Apply ${countRenames(plan.impact.renamed)}` : 'Apply changes'}
            </Button>
            {blockedReason ? (
              <p className="text-xs text-zinc-500">{blockedReason}</p>
            ) : (
              plan && (
                <p className="text-xs text-zinc-500">
                  Moves {countFiles(plan.impact.renamed)} inside {root}. No file contents change.
                </p>
              )
            )}
          </div>

          {confirming && plan && (
            <div
              role="dialog"
              aria-modal="true"
              aria-labelledby="vault-apply-title"
              className="space-y-3 rounded-lg border border-amber-700/60 bg-amber-950/30 px-4 py-3"
            >
              <h3 id="vault-apply-title" className="text-sm font-semibold text-zinc-100">
                Apply {countRenames(plan.impact.renamed)}?
              </h3>
              <p className="text-sm text-zinc-300">
                {countFiles(plan.impact.renamed)} will move inside {root}. No file contents change
                and nothing outside the Audio Vault is touched.
              </p>
              <div className="flex flex-wrap gap-2">
                <Button onClick={applyPlan} disabled={apply.isPending}>
                  Yes, move {countFiles(plan.impact.renamed)}
                </Button>
                <Button variant="secondary" onClick={() => setConfirming(false)}>
                  Cancel
                </Button>
              </div>
            </div>
          )}
        </div>
      </Card>

      {!vault.isLoading && !vault.error && items.length === 0 && (
        <div className="mt-5">
          <Card>
            <div className="py-8 text-center">
              <p className="text-sm font-medium text-zinc-200">Vault is empty</p>
              <p className="mx-auto mt-1 max-w-md text-sm text-zinc-500">
                The vault fills as approvals complete: approved donors are extracted, verified by
                SHA-256 and archived here as write-once tracks.
              </p>
            </div>
          </Card>
        </div>
      )}

      {items.length > 0 && (
        <section className="mt-5 overflow-hidden rounded-xl border border-ink-800 bg-ink-900 shadow-sm">
          <div className="overflow-x-auto">
            <table aria-label="Archived tracks" className="w-full min-w-[60rem] border-collapse text-left">
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

      <div className="mt-5">
        <Card
          title="Organise history"
          description="Every applied rename, and any run that had to be put back. A run that needs attention is listed in red — the library then shows whatever is actually on disk."
        >
          {runs.isLoading && <Spinner label="Loading organise history" />}
          {runs.error && (
            <p className="text-sm text-zinc-500">
              Could not load the organise history: {runs.error.message}
            </p>
          )}
          {!runs.isLoading && !runs.error && runRows.length === 0 && (
            <p className="text-sm text-zinc-500">No rename has been applied yet.</p>
          )}
          {runRows.length > 0 && (
            <div className="overflow-x-auto">
              <table aria-label="Organise history" className="w-full min-w-[44rem] border-collapse text-left">
                <thead>
                  <tr className="border-b border-ink-800 bg-ink-900/60 text-[11px] uppercase tracking-wider text-zinc-500">
                    <th scope="col" className="px-4 py-3 font-medium">Started</th>
                    <th scope="col" className="px-4 py-3 font-medium">Result</th>
                    <th scope="col" className="px-4 py-3 font-medium">Files</th>
                    <th scope="col" className="px-4 py-3 font-medium">Details</th>
                  </tr>
                </thead>
                <tbody>
                  {runRows.map((run) => {
                    const status = RUN_STATUS[run.status]
                    const needsAttention = run.status === 'needs_attention'
                    return (
                      <tr
                        key={run.id}
                        data-testid={`organise-run-${run.status}-${run.id}`}
                        className={
                          needsAttention
                            ? 'border-b border-red-800/60 bg-red-950/20 last:border-0'
                            : 'border-b border-ink-800/70 last:border-0'
                        }
                      >
                        <td className="whitespace-nowrap px-4 py-3 align-top text-xs text-zinc-400">
                          {createdLabel(run.started_at)}
                        </td>
                        <td className="px-4 py-3 align-top">
                          <Badge tone={status?.tone ?? 'neutral'}>{status?.label ?? run.status}</Badge>
                          {needsAttention && (
                            <p className="mt-1 max-w-md text-xs text-red-300">
                              The rename stopped part-way and could not be put back. The library now
                              lists whatever is on disk — check the file paths this run logged on the
                              Logs page before the next preview.
                            </p>
                          )}
                          {run.status === 'rolled_back' && (
                            <p className="mt-1 max-w-md text-xs text-zinc-500">
                              Every file was put back where it was, so nothing changed.
                            </p>
                          )}
                        </td>
                        <td className="whitespace-nowrap px-4 py-3 align-top text-sm text-zinc-300">
                          {run.file_count}
                        </td>
                        <td className="max-w-[18rem] px-4 py-3 align-top">
                          <span className="block break-all font-mono text-[11px] text-zinc-500">
                            {run.detail ?? '—'}
                          </span>
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      </div>
    </div>
  )
}
