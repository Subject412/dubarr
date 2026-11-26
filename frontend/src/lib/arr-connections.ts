import { ApiError, type ArrConnectionKind } from './api'

/** Human label for an arr application, used in headings, buttons and notices. */
export function arrKindLabel(kind: ArrConnectionKind): string {
  return kind === 'radarr' ? 'Radarr' : 'Sonarr'
}

export type ArrBaseUrlResult = { ok: true } | { ok: false; error: string }

const BASE_URL_ERROR = 'Base URL must start with http:// or https:// and include a host.'

/**
 * Validates an arr base URL. The scheme is required and never inferred: a bare
 * `radarr.local:7878` parses as an opaque scheme, so the protocol check rejects
 * it instead of silently turning it into `http://…`. Both IP literals and DNS
 * hostnames are accepted — only the scheme and a non-empty host are enforced.
 */
export function validateArrBaseUrl(value: string): ArrBaseUrlResult {
  const trimmed = value.trim()
  if (trimmed === '') return { ok: false, error: BASE_URL_ERROR }

  let parsed: URL
  try {
    parsed = new URL(trimmed)
  } catch {
    return { ok: false, error: BASE_URL_ERROR }
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    return { ok: false, error: BASE_URL_ERROR }
  }
  if (parsed.hostname === '') return { ok: false, error: BASE_URL_ERROR }
  return { ok: true }
}

/**
 * Display form of the server mask (`********abcd`): the last four characters
 * behind bullets. The mask is all the API ever exposes, never the plaintext.
 */
export function maskedArrKeyLabel(masked: string): string {
  const match = /^\*+([A-Za-z0-9._-]{1,4})$/.exec(masked.trim())
  return match ? `••••${match[1]}` : masked
}

/** Operator-facing message for an arr-connection failure. */
export function arrErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.status === 503) return 'Secrets are not initialised'
  return error instanceof Error ? error.message : 'Request failed'
}

/** Human labels for the server's failure classes (`class` in the 502 body). */
const ARR_FAILURE_CLASSES: Record<string, string> = {
  auth: 'authentication',
  timeout: 'timed out',
  dns: 'host not found',
  tls: 'TLS/certificate',
  status: 'unexpected status',
  kind_mismatch: 'wrong product',
  network: 'unreachable',
  decode: 'bad response',
  invalid: 'invalid address',
}

type ArrTestDiagnosticsBody = {
  class?: unknown
  status_code?: unknown
  host?: unknown
  port?: unknown
}

/**
 * Compact secondary line for a failed arr connection test, e.g.
 * `authentication (HTTP 401) · radarr:7878`. It is built only from the
 * server-classified fields — never from the raw base URL or an error body — so
 * an API key cannot be echoed back. Returns null when the server sent no
 * diagnostics (the pre-existing generic `error` message then stands alone).
 */
export function arrDiagnosticsLine(error: unknown): string | null {
  if (!(error instanceof ApiError)) return null
  const payload = error.payload
  if (!payload || typeof payload !== 'object') return null

  const body = payload as ArrTestDiagnosticsBody
  const rawClass = typeof body.class === 'string' ? body.class : ''
  if (rawClass === '') return null

  const label = ARR_FAILURE_CLASSES[rawClass] ?? rawClass
  const status = typeof body.status_code === 'number' ? body.status_code : 0
  const code = status > 0 ? ` (HTTP ${status})` : ''
  const host = typeof body.host === 'string' ? body.host : ''
  const port = typeof body.port === 'string' ? body.port : ''
  // An IPv6 literal is ambiguous without brackets (`::1:7878`); bracket it so
  // the host/port split stays readable.
  const shownHost = host.includes(':') ? `[${host}]` : host
  const target =
    shownHost === '' ? '' : port === '' ? shownHost : `${shownHost}:${port}`

  return target === '' ? `${label}${code}` : `${label}${code} · ${target}`
}
