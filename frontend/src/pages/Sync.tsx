import { useState } from 'react'
import type { LibrarySearchItem, SyncJob, SyncJobStatus, SyncReport } from '../lib/api'
import {
  useCreateSyncJob,
  useDebouncedValue,
  useLibrarySearch,
  useSyncJob,
  useSyncJobs,
  useVault,
} from '../lib/hooks'
import { Alert, Badge, Button, Card, Input, PageHeader, Spinner } from '../components/ui'
import { CheckpointChart } from '../components/CheckpointChart'

type Tone = 'neutral' | 'success' | 'warning' | 'danger' | 'accent'

const statusView: Record<SyncJobStatus, { tone: Tone; label: string }> = {
  queued: { tone: 'neutral', label: 'queued' },
  running: { tone: 'accent', label: 'running' },
  done: { tone: 'success', label: 'done' },
  failed: { tone: 'danger', label: 'failed' },
}

function statusBadge(status: string) {
  return statusView[status as SyncJobStatus] ?? { tone: 'neutral' as Tone, label: status }
}

const verdictView: Record<string, { tone: Tone; label: string }> = {
  ok: { tone: 'success', label: 'ok' },
  drifting: { tone: 'warning', label: 'drifting' },
  unreliable: { tone: 'warning', label: 'unreliable' },
  out_of_range: { tone: 'danger', label: 'out of range' },
}

function verdictBadge(verdict: string) {
  return verdictView[verdict] ?? { tone: 'neutral' as Tone, label: verdict }
}

function createdAtLabel(created?: string): string {
  if (!created) return '—'
  return created.replace('T', ' ').replace(/\.\d+Z?$/, '')
}

function formatMs(value: number): string {
  return `${Math.round(value)} ms`
}

function libraryLabel(entry: LibrarySearchItem): string {
  const base = entry.year ? `${entry.title} (${entry.year})` : entry.title
  if (entry.season_number == null || entry.episode_number == null) return base
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${base} S${pad(entry.season_number)}E${pad(entry.episode_number)}`
}

function JobRow({
  job,
  selected,
  onSelect,
}: {
  job: SyncJob
  selected: boolean
  onSelect: (id: number) => void
}) {
  const status = statusBadge(job.status)
  const verdict = job.report ? verdictBadge(job.report.verdict) : null

  return (
    <li>
      <button
        type="button"
        data-testid={`sync-job-row-${job.id}`}
        aria-current={selected ? 'true' : undefined}
        onClick={() => onSelect(job.id)}
        className={
          'flex w-full flex-wrap items-center justify-between gap-2 rounded-lg border px-3 py-2 text-left transition-colors ' +
          (selected
            ? 'border-accent-500/60 bg-accent-500/10'
            : 'border-ink-800 bg-ink-950/40 hover:border-ink-600 hover:bg-ink-800/50')
        }
      >
        <div className="flex min-w-0 flex-col gap-0.5 text-left">
          <span className="font-mono text-xs text-zinc-400">#{job.id}</span>
          {job.output_path ? (
            <span
              data-testid={`sync-job-output-${job.id}`}
              className="truncate text-sm text-zinc-200"
              title={job.output_path}
            >
              <span className="mr-1 text-[11px] uppercase tracking-wider text-zinc-500">
                Published path
              </span>{' '}
              {job.output_path}
            </span>
          ) : (
            <span className="truncate text-sm text-zinc-200" title={job.target_video_path}>
              {job.target_video_path || 'no target file'}
            </span>
          )}
          {job.output_path && job.target_video_path && job.output_path !== job.target_video_path && (
            <span className="truncate text-[11px] text-zinc-500" title={job.target_video_path}>
              Target: {job.target_video_path}
            </span>
          )}
        </div>
        <div className="flex flex-wrap items-center gap-1.5">
          <Badge tone={status.tone}>{status.label}</Badge>
          {verdict && <Badge tone={verdict.tone}>{verdict.label}</Badge>}
          <span className="text-[11px] text-zinc-500">{createdAtLabel(job.created_at)}</span>
        </div>
      </button>
    </li>
  )
}

function ReportView({ report, calculatedOffsetMs }: { report: SyncReport; calculatedOffsetMs: number }) {
  const verdict = verdictBadge(report.verdict)

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <span data-testid="verdict-badge">
          <Badge tone={verdict.tone}>{verdict.label}</Badge>
        </span>
        <Badge tone="neutral">classification: {report.classification}</Badge>
        <Badge tone="accent">method: {report.sync_method}</Badge>
        <Badge tone="neutral">applied offset: {formatMs(calculatedOffsetMs)}</Badge>
      </div>

      <dl className="grid gap-3 sm:grid-cols-4">
        <div>
          <dt className="text-[11px] uppercase tracking-wider text-zinc-500">Max drift</dt>
          <dd className="mt-0.5 text-sm text-zinc-200">{formatMs(report.max_drift_ms)}</dd>
        </div>
        <div>
          <dt className="text-[11px] uppercase tracking-wider text-zinc-500">Checkpoints</dt>
          <dd className="mt-0.5 text-sm text-zinc-200">{report.checkpoints.length}</dd>
        </div>
        <div>
          <dt className="text-[11px] uppercase tracking-wider text-zinc-500">Offsets</dt>
          <dd className="mt-0.5 font-mono text-xs text-zinc-300">
            {report.offsets_ms.map((ms) => Math.round(ms)).join(', ') || '—'}
          </dd>
        </div>
        <div>
          <dt className="text-[11px] uppercase tracking-wider text-zinc-500">Qualities</dt>
          <dd className="mt-0.5 font-mono text-xs text-zinc-300">
            {report.qualities.map((q) => q.toFixed(2)).join(', ') || '—'}
          </dd>
        </div>
      </dl>

      {report.reason && <Alert tone="warning">{report.reason}</Alert>}

      <CheckpointChart checkpoints={report.checkpoints} />

      <div className="overflow-x-auto">
        <table className="w-full text-left text-xs">
          <thead>
            <tr className="border-b border-ink-800 text-[11px] uppercase tracking-wider text-zinc-500">
              <th className="py-1.5 pr-3 font-medium">Position</th>
              <th className="py-1.5 pr-3 font-medium">Offset</th>
              <th className="py-1.5 pr-3 font-medium">Quality</th>
              <th className="py-1.5 font-medium">Slice</th>
            </tr>
          </thead>
          <tbody>
            {report.checkpoints.map((cp, index) => (
              <tr
                key={`${cp.position_pct}-${index}`}
                data-testid={`checkpoint-row-${index}`}
                className="border-b border-ink-800/60 text-zinc-300 last:border-0"
              >
                <td className="py-1.5 pr-3">{cp.position_pct}%</td>
                <td className="py-1.5 pr-3 font-mono">{formatMs(cp.offset_ms)}</td>
                <td className="py-1.5 pr-3 font-mono">{cp.correlation_quality.toFixed(2)}</td>
                <td className="py-1.5 text-zinc-500">
                  {cp.slice_start_sec.toFixed(1)}s + {cp.slice_len_sec.toFixed(1)}s
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

function JobDetail({
  jobId,
  onRerun,
  rerunning,
}: {
  jobId: number
  onRerun: (job: SyncJob) => void
  rerunning: boolean
}) {
  const job = useSyncJob(jobId)

  if (job.isLoading) return <Spinner label="Loading sync job" />
  if (job.error) return <Alert>Could not load sync job #{jobId}: {job.error.message}</Alert>
  if (!job.data) return null

  const data = job.data
  const status = statusBadge(data.status)
  const canRerun = data.status === 'failed' && !rerunning
  const showTargetFile = Boolean(
    data.output_path && data.target_video_path && data.output_path !== data.target_video_path,
  )

  return (
    <Card
      title={`Job #${data.id}`}
      description={
        data.output_path ? (
          <span data-testid="sync-detail-output">
            Published path: <span className="font-mono">{data.output_path}</span>
          </span>
        ) : (
          <span data-testid="sync-detail-target">
            {data.target_video_path || 'No target file recorded'}
          </span>
        )
      }
      actions={
        <>
          <span data-testid="sync-status">
            <Badge tone={status.tone}>{status.label}</Badge>
          </span>
          {canRerun && (
            <Button variant="secondary" size="sm" onClick={() => onRerun(data)}>
              Re-run
            </Button>
          )}
        </>
      }
    >
      <div className="space-y-4">
        <dl className="grid gap-3 sm:grid-cols-3">
          <div>
            <dt className="text-[11px] uppercase tracking-wider text-zinc-500">Vault track</dt>
            <dd className="mt-0.5 text-sm text-zinc-200">#{data.vault_track_id}</dd>
          </div>
          <div>
            <dt className="text-[11px] uppercase tracking-wider text-zinc-500">Library target</dt>
            <dd className="mt-0.5 text-sm text-zinc-200">#{data.target_library_cache_id}</dd>
          </div>
          <div>
            <dt className="text-[11px] uppercase tracking-wider text-zinc-500">Target language</dt>
            <dd className="mt-0.5 text-sm text-zinc-200">{data.target_lang_iso2 || '—'}</dd>
          </div>
          {showTargetFile && (
            <div className="sm:col-span-3">
              <dt className="text-[11px] uppercase tracking-wider text-zinc-500">Target file</dt>
              <dd data-testid="sync-detail-target" className="mt-0.5 break-all font-mono text-xs text-zinc-300">
                {data.target_video_path}
              </dd>
            </div>
          )}
        </dl>

        {data.status === 'failed' && data.error_message && (
          <Alert>Sync failed: {data.error_message}</Alert>
        )}

        {data.report ? (
          <ReportView report={data.report} calculatedOffsetMs={data.calculated_offset_ms} />
        ) : (
          <p className="text-sm text-zinc-500">
            {data.status === 'done'
              ? 'This job finished without a report.'
              : 'Waiting for the analysis worker to persist a report…'}
          </p>
        )}
      </div>
    </Card>
  )
}

function ExecutePanel({ onCreated }: { onCreated: (job: SyncJob) => void }) {
  const vault = useVault()
  const [vaultTrackId, setVaultTrackId] = useState<number | null>(null)
  const [libraryQuery, setLibraryQuery] = useState('')
  const [libraryTarget, setLibraryTarget] = useState<LibrarySearchItem | null>(null)
  const debounced = useDebouncedValue(libraryQuery, 300)
  const search = useLibrarySearch(debounced)
  const create = useCreateSyncJob()
  const tooShort = debounced.trim().length < 2

  const execute = () => {
    if (vaultTrackId == null || libraryTarget == null) return
    create.mutate(
      { vault_track_id: vaultTrackId, target_library_cache_id: libraryTarget.id },
      { onSuccess: onCreated },
    )
  }

  return (
    <Card title="Execute" description="Queue a multi-point sync analysis for one vault track.">
      <div className="grid gap-4 sm:grid-cols-2">
        <label className="space-y-1.5">
          <span className="block text-sm font-medium text-zinc-200">Vault track</span>
          <select
            aria-label="Vault track"
            className="w-full rounded-lg border border-ink-700 bg-ink-950/70 px-3 py-2 text-sm text-zinc-100 focus:border-accent-500 focus:outline-none"
            value={vaultTrackId ?? ''}
            onChange={(event) =>
              setVaultTrackId(event.target.value === '' ? null : Number(event.target.value))
            }
          >
            <option value="">Select a vault track…</option>
            {(vault.data?.items ?? []).map((item) => (
              <option key={item.id} value={item.id}>
                #{item.id} · {item.title} · {item.language}
              </option>
            ))}
          </select>
          {vault.isLoading && <p className="text-xs text-zinc-500">Loading vault…</p>}
          {vault.data && vault.data.items.length === 0 && (
            <p className="text-xs text-zinc-500">Vault is empty.</p>
          )}
        </label>

        <div className="space-y-1.5">
          <span className="block text-sm font-medium text-zinc-200">Library target</span>
          <Input
            aria-label="Library target search"
            placeholder="Search the arr library…"
            autoComplete="off"
            value={libraryQuery}
            onChange={(event) => setLibraryQuery(event.target.value)}
          />
          {libraryTarget ? (
            <div className="flex items-center justify-between gap-2 rounded-md border border-ink-700 bg-ink-950/60 px-3 py-1.5 text-sm">
              <span className="truncate text-zinc-200">{libraryLabel(libraryTarget)}</span>
              <span className="shrink-0 text-xs text-zinc-500">#{libraryTarget.id}</span>
            </div>
          ) : (
            <>
              {search.isFetching && <p className="text-xs text-zinc-500">Searching…</p>}
              {search.error && (
                <p className="text-xs text-red-400">Library search failed: {search.error.message}</p>
              )}
              {!tooShort && !search.isFetching && search.data?.items.length === 0 && (
                <p className="text-xs text-zinc-500">No library matches.</p>
              )}
              {!tooShort && search.data && search.data.items.length > 0 && (
                <ul className="max-h-40 space-y-1 overflow-y-auto">
                  {search.data.items.map((entry) => (
                    <li key={entry.id}>
                      <button
                        type="button"
                        data-testid={`library-result-${entry.id}`}
                        onClick={() => setLibraryTarget(entry)}
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
            </>
          )}
        </div>
      </div>

      <div className="mt-4 flex flex-wrap items-center justify-between gap-3">
        {create.error ? (
          <p className="text-xs text-red-400">Could not create the job: {create.error.message}</p>
        ) : (
          <p className="text-xs text-zinc-500">
            Queues a job; status polls automatically while it is queued or running.
          </p>
        )}
        <Button
          data-testid="sync-execute"
          onClick={execute}
          disabled={vaultTrackId == null || libraryTarget == null || create.isPending}
        >
          {create.isPending ? 'Queuing…' : 'Execute sync'}
        </Button>
      </div>
    </Card>
  )
}

export function SyncPage() {
  const jobs = useSyncJobs()
  const [selectedId, setSelectedId] = useState<number | null>(null)
  const create = useCreateSyncJob()
  const items = jobs.data?.items ?? []
  // Land on the newest job (server order) until the operator picks another.
  const activeId = selectedId ?? items[0]?.id ?? null

  const rerun = (job: SyncJob) => {
    create.mutate(
      { vault_track_id: job.vault_track_id, target_library_cache_id: job.target_library_cache_id },
      { onSuccess: (created) => setSelectedId(created.id) },
    )
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title="Sync Studio"
        description="Queue sync analyses, watch drift checkpoints land, and review the verdict before muxing. Unreliable or out-of-range reports fail loudly."
        actions={
          <>
            <Badge tone={items.length > 0 ? 'accent' : 'neutral'}>{items.length} jobs</Badge>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => void jobs.refetch()}
              disabled={jobs.isFetching}
            >
              {jobs.isFetching ? 'Refreshing…' : 'Refresh'}
            </Button>
          </>
        }
      />

      <ExecutePanel onCreated={(job) => setSelectedId(job.id)} />

      <Card title="Jobs" description="Newest first.">
        {jobs.isLoading && <Spinner label="Loading sync jobs" />}
        {jobs.error && <Alert>Could not load sync jobs: {jobs.error.message}</Alert>}
        {!jobs.isLoading && !jobs.error && items.length === 0 && (
          <p className="py-6 text-center text-sm text-zinc-500">
            No sync jobs yet. Pick a vault track and a library target above and execute.
          </p>
        )}
        {items.length > 0 && (
          <ul className="space-y-2" data-testid="sync-job-list">
            {items.map((job) => (
              <JobRow
                key={job.id}
                job={job}
                selected={activeId === job.id}
                onSelect={setSelectedId}
              />
            ))}
          </ul>
        )}
      </Card>

      {activeId !== null && (
        <JobDetail jobId={activeId} onRerun={rerun} rerunning={create.isPending} />
      )}
    </div>
  )
}
