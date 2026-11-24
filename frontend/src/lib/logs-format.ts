/** Human-readable age of a log line; falls back to the date for old entries. */
export function formatRelative(ts: string, now: Date = new Date()): string {
  const time = new Date(ts)
  if (Number.isNaN(time.getTime())) return ts
  const diff = now.getTime() - time.getTime()
  if (diff < 60_000) return `${Math.max(0, Math.round(diff / 1000))}s ago`
  if (diff < 3_600_000) return `${Math.round(diff / 60_000)}m ago`
  if (diff < 86_400_000) return `${Math.round(diff / 3_600_000)}h ago`
  return time.toISOString().slice(0, 10)
}

/** One line of the visible tail: timestamp, level, component, message. */
export function formatLogEntry(entry: {
  ts: string
  level: string
  component?: string
  message: string
  job_id?: string
  media_id?: string
}): string {
  const parts = [entry.ts, entry.level.toUpperCase()]
  if (entry.component) parts.push(entry.component)
  if (entry.job_id) parts.push(`job=${entry.job_id}`)
  if (entry.media_id) parts.push(`media=${entry.media_id}`)
  return `${parts.join(' ')} ${entry.message}`
}
