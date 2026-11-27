import { useState } from 'react'
import type {
  LibrarySearchItem,
  SyncAttempt,
  SyncCheckpoint,
  SyncFit,
  SyncJob,
  SyncJobStatus,
  SyncReport,
  SyncStrategy,
  SyncTechnique,
} from '../lib/api'
import {
  useCreateSyncJob,
  useDebouncedValue,
  useLibrarySearch,
  useSyncJob,
  useSyncJobs,
  useVault,
} from '../lib/hooks'
import { Alert, Badge, Button, Card, InfoTip, Input, PageHeader, Spinner } from '../components/ui'
import { CheckpointChart } from '../components/CheckpointChart'
import { helpIdFor } from '../lib/help-id'
import { presetLabel } from '../lib/strategy'

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

/**
 * The slope reads in milliseconds of offset per second of film, the unit the
 * backend reports (`slope_ms_per_sec`): 40.96 is the 25.000 -> 23.976 fps
 * mismatch (1 − 23.976/25 = 0.04096 s of drift per second of film).
 */
function formatSlope(value: number): string {
  return `${value < 0 ? '−' : '+'}${Math.abs(value).toFixed(1)} ms/s`
}

/**
 * The plain word for a technique id (`analyze.TechniqueID`). An id this build
 * does not know still renders as itself: the report is the record of what ran,
 * so hiding an unknown method would be a lie.
 */
const TECHNIQUE_LABELS: Record<string, string> = {
  waveform: 'Waveform',
  mfcc: 'MFCC',
}

function techniqueLabel(id?: string): string {
  if (!id) return '—'
  return TECHNIQUE_LABELS[id] ?? id
}

/**
 * At or above this, a reported precision is a grid rather than a sample period:
 * the waveform matcher answers on the 0.125 ms sample while a hop-based matcher
 * answers on its hop (MFCC: 10 ms). D3 forbids presenting a hop-quantised
 * offset as sample-accurate, so from here up the badge names the grid and the
 * technique that answered on it.
 */
const HOP_QUANTISED_PRECISION_MS = 5

type ReportPrecision = {
  /** The report's own precision, in milliseconds. */
  ms: number
  /** The technique that answered on the grid; empty for sample precision. */
  technique: string
}

/**
 * How the report's offset accuracy has to be stated. `precision_ms` is the
 * coarsest precision among the checkpoints, so a coarse value means at least
 * one answer sits on a hop grid, and the technique named is the one that set
 * it. A waveform report keeps its plain number; a 0.4.0 report (no precision
 * recorded) renders no badge at all.
 */
function reportPrecision(report: SyncReport): ReportPrecision | null {
  const ms = report.precision_ms
  if (ms == null || ms <= 0) return null
  if (ms < HOP_QUANTISED_PRECISION_MS) return { ms, technique: '' }
  const coarse = report.checkpoints.find((cp) => (cp.precision_ms ?? 0) >= HOP_QUANTISED_PRECISION_MS)
  return { ms, technique: coarse?.technique ? techniqueLabel(coarse.technique) : '' }
}

/** The badge text: a sample period as itself, a hop grid named with its matcher. */
function precisionLabel(precision: ReportPrecision): string {
  return precision.technique
    ? `±${precision.ms} ms · ${precision.technique}`
    : `±${precision.ms} ms`
}

/** Why a strategy step runs, in the words the settings tooltip uses. */
const WHEN_LABELS: Record<string, string> = {
  always: 'always',
  below_confidence: 'when the previous step was unsure',
  above_drift: 'on checkpoints that break the trend',
}

function whenLabel(when: string): string {
  return WHEN_LABELS[when] ?? when
}

/**
 * Why an attempt did not decide its checkpoint. The gates come from the
 * strategy the report recorded, so the sentence is the one the operator's own
 * thresholds produced; without a matching step the reason stays generic.
 */
function attemptOutcome(attempt: SyncAttempt, step?: SyncTechnique): string {
  if (attempt.accepted) return 'accepted'
  if (attempt.error) return `rejected: ${attempt.error}`
  if (!step) return 'rejected'
  const gates: string[] = []
  if (attempt.quality < step.min_confidence) {
    gates.push(`below the confidence gate (${step.min_confidence.toFixed(2)})`)
  }
  if (step.min_match_z > 0 && attempt.match_z > 0 && attempt.match_z < step.min_match_z) {
    gates.push(`peak under the z gate (${step.min_match_z.toFixed(1)}σ)`)
  }
  return gates.length > 0 ? `rejected: ${gates.join(' · ')}` : 'rejected'
}

/**
 * What the job ran (#82), above the checkpoint table: the starting point and
 * the steps in list order, plus any note the report left about a technique it
 * could not run. Both are 0.5.0-only, so a 0.4.0 report renders nothing here.
 */
function StrategySummary({ strategy, notes }: { strategy?: SyncStrategy; notes?: string[] }) {
  const hasNotes = (notes?.length ?? 0) > 0
  if (!strategy && !hasNotes) return null
  const steps = strategy?.techniques ?? []
  const preset = strategy ? presetLabel(strategy.preset) : ''

  return (
    <div className="space-y-1">
      {strategy && (
        <p data-testid="sync-strategy-summary" className="text-xs text-zinc-400">
          <span className="text-[11px] uppercase tracking-wider text-zinc-500">Strategy</span>{' '}
          <span className="text-zinc-200">{preset}</span>
          {steps.length > 0 && (
            <span className="text-zinc-500">
              {' · '}
              {steps.map((step) => `${techniqueLabel(step.id)} ${whenLabel(step.when)}`).join(' → ')}
            </span>
          )}
        </p>
      )}
      {hasNotes && (
        <ul data-testid="sync-strategy-notes" className="space-y-0.5 text-[11px] text-zinc-500">
          {(notes ?? []).map((note, index) => (
            <li key={`${index}-${note}`}>{note}</li>
          ))}
        </ul>
      )}
    </div>
  )
}

/**
 * One checkpoint row, plus — when more than one technique ran here — a
 * disclosure listing every attempt so the operator can see why a second method
 * was tried. Collapsed by default: a single-method checkpoint, which is the
 * whole of a 0.4.0 report and of the shipped strategy, stays one line.
 */
function CheckpointRow({
  checkpoint,
  index,
  showMethod,
  strategy,
}: {
  checkpoint: SyncCheckpoint
  index: number
  showMethod: boolean
  strategy?: SyncStrategy
}) {
  const [open, setOpen] = useState(false)
  const attempts = checkpoint.attempts ?? []
  const expandable = attempts.length > 1

  return (
    <>
      <tr
        data-testid={`checkpoint-row-${index}`}
        className="border-b border-ink-800/60 text-zinc-300 last:border-0"
      >
        <td className="py-1.5 pr-3">{checkpoint.position_pct}%</td>
        <td className="py-1.5 pr-3 font-mono">{formatMs(checkpoint.offset_ms)}</td>
        <td className="py-1.5 pr-3 font-mono">{checkpoint.correlation_quality.toFixed(2)}</td>
        {showMethod && (
          <td data-testid={`checkpoint-method-${index}`} className="py-1.5 pr-3">
            {techniqueLabel(checkpoint.technique)}
            {expandable && (
              <button
                type="button"
                data-testid={`checkpoint-attempts-toggle-${index}`}
                aria-expanded={open}
                onClick={() => setOpen(!open)}
                className="ml-2 text-[11px] text-accent-400 underline-offset-2 hover:underline"
              >
                {open ? 'hide' : `${attempts.length} methods`}
              </button>
            )}
          </td>
        )}
        <td className="py-1.5 pr-3 font-mono">
          {checkpoint.match_z ? checkpoint.match_z.toFixed(1) : '—'}
        </td>
        <td className="py-1.5 text-zinc-500">
          {checkpoint.slice_start_sec.toFixed(1)}s + {checkpoint.slice_len_sec.toFixed(1)}s
        </td>
      </tr>
      {expandable && open && (
        <tr
          data-testid={`checkpoint-attempts-${index}`}
          className="border-b border-ink-800/60 last:border-0"
        >
          <td colSpan={showMethod ? 6 : 5} className="py-1.5">
            <ul className="space-y-0.5 pl-3 text-[11px] text-zinc-500">
              {attempts.map((attempt, attemptIndex) => (
                <li
                  key={`${attempt.technique}-${attemptIndex}`}
                  data-testid={`checkpoint-attempt-${index}-${attemptIndex}`}
                >
                  <span className="text-zinc-300">{techniqueLabel(attempt.technique)}</span>{' '}
                  <span className="font-mono">{formatMs(attempt.offset_ms)}</span>
                  {' · '}confidence {attempt.quality.toFixed(2)}
                  {' · '}z {attempt.match_z ? attempt.match_z.toFixed(1) : '—'}
                  {' · '}
                  {attemptOutcome(
                    attempt,
                    strategy?.techniques.find((step) => step.id === attempt.technique),
                  )}
                </li>
              ))}
            </ul>
          </td>
        </tr>
      )}
    </>
  )
}

function libraryLabel(entry: LibrarySearchItem): string {
  const base = entry.year ? `${entry.title} (${entry.year})` : entry.title
  if (entry.season_number == null || entry.episode_number == null) return base
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${base} S${pad(entry.season_number)}E${pad(entry.episode_number)}`
}

const shapeView: Record<string, { tone: Tone; label: string }> = {
  constant: { tone: 'success', label: 'constant' },
  drift: { tone: 'warning', label: 'drift' },
  cut: { tone: 'danger', label: 'cut' },
  mixed: { tone: 'neutral', label: 'mixed' },
  insufficient: { tone: 'neutral', label: 'insufficient' },
}

/**
 * D11: what the trend means, when it matters and what it costs. Behind `shape`
 * it can only appear on a report that actually carries a fit.
 */
const TREND_HELP =
  "A steady slope across the run means the dub was made at a slightly different " +
  'frame rate. Dubarr corrects it by stretching the audio to the film’s speed ' +
  '(pitch is preserved) and re-encodes the dub copy once, losslessly. A step ' +
  'instead of a slope means the film was re-cut, which this release cannot fix ' +
  'automatically.'

/**
 * The line fit through the checkpoint offsets (#86). Every number is optional on
 * the wire: a 0.4.0 report has no `fit` at all and renders a dash, never a
 * fabricated zero slope.
 */
function TrendBlock({ fit }: { fit?: SyncFit }) {
  const shape = fit?.shape
  const shapeBadge = shape ? (shapeView[shape] ?? { tone: 'neutral' as Tone, label: shape }) : null
  const slope = fit?.slope_ms_per_sec
  const r2 = fit?.r2
  const tempo = fit?.tempo_factor
  const cutAt = fit?.cut_at_sec
  const cutUncertainty = fit?.cut_uncertainty_sec

  return (
    <section data-testid="sync-trend" className="space-y-2">
      <div className="flex items-center gap-1.5">
        <h3 className="text-[11px] uppercase tracking-wider text-zinc-500">Trend</h3>
        <InfoTip id={helpIdFor('Trend')} label="Trend" text={TREND_HELP} />
      </div>
      {fit ? (
        <dl className="grid gap-3 sm:grid-cols-4">
          <div>
            <dt className="text-[11px] uppercase tracking-wider text-zinc-500">Shape</dt>
            <dd className="mt-0.5 text-sm text-zinc-200">
              <span data-testid="sync-trend-shape">
                <Badge tone={shapeBadge?.tone ?? 'neutral'}>{shapeBadge?.label ?? '—'}</Badge>
              </span>
            </dd>
          </div>
          <div>
            <dt className="text-[11px] uppercase tracking-wider text-zinc-500">Slope</dt>
            <dd data-testid="sync-trend-slope" className="mt-0.5 font-mono text-sm text-zinc-200">
              {typeof slope === 'number' && Number.isFinite(slope) ? formatSlope(slope) : '—'}
            </dd>
          </div>
          <div>
            <dt className="text-[11px] uppercase tracking-wider text-zinc-500">R²</dt>
            <dd data-testid="sync-trend-r2" className="mt-0.5 font-mono text-sm text-zinc-200">
              {typeof r2 === 'number' && Number.isFinite(r2) ? r2.toFixed(3) : '—'}
            </dd>
          </div>
          <div>
            <dt className="text-[11px] uppercase tracking-wider text-zinc-500">Tempo</dt>
            <dd data-testid="sync-trend-tempo" className="mt-0.5 font-mono text-sm text-zinc-200">
              {typeof tempo === 'number' && Number.isFinite(tempo) && tempo > 0
                ? `tempo ${tempo.toFixed(4)} · ${fit.tempo_applied ? 'applied' : 'not applied'}`
                : '—'}
            </dd>
          </div>
          {shape === 'cut' && typeof cutAt === 'number' && Number.isFinite(cutAt) && (
            <div className="sm:col-span-2">
              <dt className="text-[11px] uppercase tracking-wider text-zinc-500">Cut</dt>
              <dd data-testid="sync-trend-cut" className="mt-0.5 font-mono text-sm text-zinc-200">
                {`cut at ${cutAt.toFixed(1)} s${
                  typeof cutUncertainty === 'number' && Number.isFinite(cutUncertainty)
                    ? ` ± ${cutUncertainty.toFixed(1)} s`
                    : ''
                }`}
              </dd>
            </div>
          )}
        </dl>
      ) : (
        <p data-testid="sync-trend-empty" className="text-sm text-zinc-300">
          —
        </p>
      )}
    </section>
  )
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
  // What was done to the dub copy before the mux (#86): a tempo re-encode is
  // otherwise only inferable from an applied offset of ~0.
  const applicationNote = job.report?.application?.note

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
          {applicationNote && (
            <span
              data-testid={`sync-job-application-${job.id}`}
              className="truncate text-[11px] text-accent-400"
              title={applicationNote}
            >
              {applicationNote}
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
  const precision = reportPrecision(report)
  // A recorded method is new in 0.5.0. A report that records neither a
  // technique nor a second attempt anywhere keeps the five-column table it was
  // stored with: a column of dashes would change a 0.4.0 report's shape for no
  // information. One checkpoint naming a method is enough to earn the column.
  const showMethod = report.checkpoints.some(
    (cp) => Boolean(cp.technique) || (cp.attempts?.length ?? 0) > 1,
  )

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <span data-testid="verdict-badge">
          <Badge tone={verdict.tone}>{verdict.label}</Badge>
        </span>
        <Badge tone="neutral">classification: {report.classification}</Badge>
        <Badge tone="accent">method: {report.sync_method}</Badge>
        <Badge tone="neutral">applied offset: {formatMs(calculatedOffsetMs)}</Badge>
        {precision && (
          <span data-testid="sync-precision">
            <Badge tone="neutral">{precisionLabel(precision)}</Badge>
          </span>
        )}
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

      <TrendBlock fit={report.fit} />

      {report.reason && <Alert tone="warning">{report.reason}</Alert>}

      <CheckpointChart checkpoints={report.checkpoints} fit={report.fit} />

      <StrategySummary strategy={report.strategy} notes={report.notes} />

      <div className="overflow-x-auto">
        <table className="w-full text-left text-xs">
          <thead>
            <tr className="border-b border-ink-800 text-[11px] uppercase tracking-wider text-zinc-500">
              <th className="py-1.5 pr-3 font-medium">Position</th>
              <th className="py-1.5 pr-3 font-medium">Offset</th>
              <th className="py-1.5 pr-3 font-medium">Quality</th>
              {showMethod && <th className="py-1.5 pr-3 font-medium">Method</th>}
              <th className="py-1.5 pr-3 font-medium">Match z</th>
              <th className="py-1.5 font-medium">Slice</th>
            </tr>
          </thead>
          <tbody>
            {report.checkpoints.map((cp, index) => (
              <CheckpointRow
                key={`${cp.position_pct}-${index}`}
                checkpoint={cp}
                index={index}
                showMethod={showMethod}
                strategy={report.strategy}
              />
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
          <div>
            <dt className="text-[11px] uppercase tracking-wider text-zinc-500">Analysis</dt>
            <dd data-testid="sync-detail-analysis" className="mt-0.5 text-sm text-zinc-200">
              {data.analysis_source || '—'}
              {data.analysis_fallback ? ` (fallback: ${data.analysis_fallback})` : ''}
            </dd>
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
