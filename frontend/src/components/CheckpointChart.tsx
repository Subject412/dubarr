import type { SyncBoundary, SyncCheckpoint, SyncFit } from '../lib/api'

const WIDTH = 640
const HEIGHT = 220
const LEFT = 56
const RIGHT = 20
const TOP = 18
// The lower band is reserved for the per-checkpoint correlation quality bars.
const BOTTOM = 56
const QUALITY_HEIGHT = 26

function clampPosition(pct: number): number {
  if (!Number.isFinite(pct)) return 0
  return Math.min(100, Math.max(0, pct))
}

/**
 * The film's length, recovered from the checkpoints themselves: each one reports
 * the centre of its slice in both units (`position_pct` is that centre as a
 * percentage of the film, `slice_start_sec`/`slice_len_sec` are that same centre
 * in seconds), so `(start + len/2)·100/pct` is the duration. A boundary arrives
 * in film seconds while this chart's x-axis is percent, and the report carries
 * no duration of its own — hence the median estimate rather than a new field.
 */
function filmDurationSec(checkpoints: SyncCheckpoint[]): number | null {
  const estimates = checkpoints
    .filter(
      (cp) =>
        cp.position_pct > 0 &&
        Number.isFinite(cp.slice_start_sec) &&
        Number.isFinite(cp.slice_len_sec),
    )
    .map((cp) => ((cp.slice_start_sec + cp.slice_len_sec / 2) * 100) / cp.position_pct)
    .filter((value) => Number.isFinite(value) && value > 0)
    .sort((a, b) => a - b)
  if (estimates.length === 0) return null
  // Clamped windows at either end make a single estimate sloppy; the median is
  // the middle of the film's own evidence and is exact for a dense plan.
  return estimates[Math.floor(estimates.length / 2)]
}

/**
 * Checkpoint timeline for the Sync Studio. Correlation offsets are drawn as a
 * line of dots against position%; quality is a bar under each dot.
 *
 * When the report carries a line fit (#86), the fitted trend is drawn as a
 * dashed grey polyline over the dots: y = intercept_ms + slope_ms_per_sec ·
 * slice_start_sec, at each checkpoint's own x. A fit the analysis could not
 * classify (`insufficient`, fewer than three points) has no line to draw, and
 * neither does a 0.4.0 report — the dots are unchanged either way.
 *
 * Deliberate deviation from PLAN.md M5-2: there is no API endpoint that
 * streams a vault .mka, and wavesurfer.js is not a dependency, so an audio
 * waveform would have nothing to play. This pure-SVG chart carries the same
 * per-checkpoint information (offset + quality) with zero new dependencies.
 */
export function CheckpointChart({
  checkpoints,
  fit,
  boundaries,
}: {
  checkpoints: SyncCheckpoint[]
  fit?: SyncFit
  /**
   * Accepted joins from a piecewise plan (#84). Each is drawn as a dashed
   * vertical marker in film time; a rejected plan's boundaries are not evidence
   * and the caller passes none, so nothing is drawn for a plan nobody trusts.
   */
  boundaries?: SyncBoundary[]
}) {
  if (checkpoints.length === 0) {
    return <p className="text-sm text-zinc-500">No checkpoints in this report.</p>
  }

  const plotWidth = WIDTH - LEFT - RIGHT
  const plotHeight = HEIGHT - TOP - BOTTOM
  const maxOffset = Math.max(1, ...checkpoints.map((cp) => Math.abs(cp.offset_ms)))
  const x = (pct: number) => LEFT + (clampPosition(pct) / 100) * plotWidth
  const y = (offset: number) => TOP + plotHeight / 2 - (offset / maxOffset) * (plotHeight / 2)
  const zeroY = y(0)

  // The fitted trend, if the report has one the analysis could classify. The
  // scale stays the dots' scale, so nothing about their geometry moves.
  const slope = fit?.slope_ms_per_sec
  const intercept = fit?.intercept_ms
  const trend =
    fit &&
    fit.shape !== 'insufficient' &&
    typeof slope === 'number' &&
    Number.isFinite(slope) &&
    typeof intercept === 'number' &&
    Number.isFinite(intercept)
      ? { slope, intercept }
      : null
  const fitPoints = trend
    ? [...checkpoints]
        .filter((cp) => Number.isFinite(cp.slice_start_sec))
        .sort((a, b) => a.slice_start_sec - b.slice_start_sec)
        .map(
          (cp) =>
            `${x(cp.position_pct)},${y(trend.intercept + trend.slope * cp.slice_start_sec)}`,
        )
    : []
  const fitLine = fitPoints.length >= 2 ? fitPoints.join(' ') : null

  // Accepted joins, on the same x scale as the dots. Nothing else about the
  // plot moves: these are markers, not a second geometry.
  const duration = filmDurationSec(checkpoints)
  const boundaryMarkers =
    duration == null
      ? []
      : (boundaries ?? [])
          .filter((boundary) => Number.isFinite(boundary.at_sec))
          .map((boundary) => ({
            at: boundary.at_sec,
            pct: (boundary.at_sec / duration) * 100,
            uncertainty: Number.isFinite(boundary.uncertainty_sec) ? boundary.uncertainty_sec : 0,
          }))

  return (
    <figure className="w-full">
      <svg
        data-testid="checkpoint-chart"
        role="img"
        aria-label={`Checkpoint timeline with ${checkpoints.length} checkpoints`}
        viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
        className="w-full"
      >
        <rect x={LEFT} y={TOP} width={plotWidth} height={plotHeight} fill="#0b0e13" />
        {[0, 25, 50, 75, 100].map((tick) => (
          <g key={`x-${tick}`}>
            <line
              x1={x(tick)}
              x2={x(tick)}
              y1={TOP}
              y2={TOP + plotHeight}
              stroke="#212936"
              strokeWidth={1}
            />
            <text
              x={x(tick)}
              y={TOP + plotHeight + 16}
              textAnchor="middle"
              fontSize={10}
              fill="#71717a"
            >
              {tick}%
            </text>
          </g>
        ))}
        <line
          x1={LEFT}
          x2={LEFT + plotWidth}
          y1={zeroY}
          y2={zeroY}
          stroke="#3f3f46"
          strokeDasharray="4 4"
        />
        <text x={LEFT - 8} y={y(maxOffset) + 4} textAnchor="end" fontSize={10} fill="#71717a">
          +{Math.round(maxOffset)}ms
        </text>
        <text x={LEFT - 8} y={zeroY + 4} textAnchor="end" fontSize={10} fill="#71717a">
          0
        </text>
        <text x={LEFT - 8} y={y(-maxOffset) + 4} textAnchor="end" fontSize={10} fill="#71717a">
          −{Math.round(maxOffset)}ms
        </text>

        <polyline
          points={checkpoints.map((cp) => `${x(cp.position_pct)},${y(cp.offset_ms)}`).join(' ')}
          fill="none"
          stroke="#2dd4bf"
          strokeWidth={2}
        />

        {fitLine && (
          <polyline
            data-testid="fit-line"
            points={fitLine}
            fill="none"
            stroke="#a1a1aa"
            strokeWidth={1.5}
            strokeDasharray="6 4"
          />
        )}

        {/*
          A join is drawn under the dots so it cannot hide one: where the dub
          was re-cut, the offset steps by thousands of milliseconds and the
          dots either side are the evidence for it.
        */}
        {boundaryMarkers.map((marker, index) => (
          <line
            key={`boundary-${index}`}
            data-testid={`boundary-marker-${index}`}
            data-at-sec={marker.at}
            x1={x(marker.pct)}
            x2={x(marker.pct)}
            y1={TOP}
            y2={TOP + plotHeight}
            stroke="#f59e0b"
            strokeWidth={1.5}
            strokeDasharray="5 4"
          >
            <title>{`join at ${marker.at.toFixed(1)} s ± ${marker.uncertainty.toFixed(1)} s`}</title>
          </line>
        ))}

        {checkpoints.map((cp, index) => {
          const quality = Math.min(1, Math.max(0, cp.correlation_quality))
          const barHeight = Math.max(1, quality * QUALITY_HEIGHT)
          return (
            <g key={`${cp.position_pct}-${index}`}>
              <circle
                data-testid={`checkpoint-dot-${index}`}
                data-offset-ms={cp.offset_ms}
                cx={x(cp.position_pct)}
                cy={y(cp.offset_ms)}
                r={5}
                fill="#5eead4"
                stroke="#0b0e13"
                strokeWidth={1.5}
              >
                <title>
                  {`position ${cp.position_pct}% · offset ${Math.round(cp.offset_ms)}ms · quality ${cp.correlation_quality.toFixed(2)}${
                    cp.match_z ? ` · z ${cp.match_z.toFixed(1)}` : ''
                  }`}
                </title>
              </circle>
              <rect
                data-testid={`checkpoint-quality-${index}`}
                x={x(cp.position_pct) - 9}
                y={TOP + plotHeight + QUALITY_HEIGHT - barHeight + 6}
                width={18}
                height={barHeight}
                rx={2}
                fill={quality >= 0.5 ? '#2dd4bf' : '#f59e0b'}
                opacity={0.7}
              >
                <title>{`quality ${cp.correlation_quality.toFixed(2)}`}</title>
              </rect>
            </g>
          )
        })}
        <text x={LEFT} y={HEIGHT - 6} fontSize={10} fill="#71717a">
          offset per checkpoint (top) · correlation quality (bottom bars)
        </text>
      </svg>
    </figure>
  )
}
