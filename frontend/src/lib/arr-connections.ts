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
