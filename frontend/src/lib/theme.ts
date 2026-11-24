import { createContext, useCallback, useContext, useEffect, useState } from 'react'

/**
 * Theme preference is a per-browser setting, not appliance config: it must
 * apply to /login and /setup before any authenticated settings fetch (ADR-0003
 * keeps app config on the server, this is deliberately client-only).
 */
export type ThemePreference = 'light' | 'dark' | 'system'
export type ResolvedTheme = 'light' | 'dark'

export const THEME_STORAGE_KEY = 'dubarr.theme'
export const DEFAULT_THEME: ThemePreference = 'dark'
export const THEME_PREFERENCES: readonly ThemePreference[] = ['light', 'dark', 'system']

export type ThemeState = {
  preference: ThemePreference
  resolved: ResolvedTheme
  setPreference: (next: ThemePreference) => void
}

function isThemePreference(value: unknown): value is ThemePreference {
  return value === 'light' || value === 'dark' || value === 'system'
}

/** localStorage can throw (privacy mode, disabled storage) — never break boot. */
function storage(): Storage | null {
  try {
    return typeof window === 'undefined' ? null : window.localStorage
  } catch {
    return null
  }
}

export function readStoredTheme(): ThemePreference {
  try {
    const raw = storage()?.getItem(THEME_STORAGE_KEY)
    if (isThemePreference(raw)) return raw
  } catch {
    // fall through to the default
  }
  return DEFAULT_THEME
}

export function storeTheme(preference: ThemePreference): void {
  try {
    storage()?.setItem(THEME_STORAGE_KEY, preference)
  } catch {
    // A blocked storage must not stop the selection from applying this session.
  }
}

/** Dark when the OS prefers dark or when matchMedia is unavailable (the default). */
export function prefersDark(): boolean {
  try {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return true
    return window.matchMedia('(prefers-color-scheme: dark)').matches
  } catch {
    return true
  }
}

export function resolveTheme(
  preference: ThemePreference,
  systemDark = prefersDark(),
): ResolvedTheme {
  if (preference === 'system') return systemDark ? 'dark' : 'light'
  return preference
}

/** The single place that writes the resolved theme to the document. */
export function applyResolvedTheme(theme: ResolvedTheme): void {
  if (typeof document === 'undefined') return
  document.documentElement.dataset.theme = theme
  // Mirror data-theme so native controls and scrollbars match the palette.
  document.documentElement.style.colorScheme = theme
}

export const ThemeContext = createContext<ThemeState | null>(null)

/**
 * Preference state shared by ThemeProvider and detached consumers. `track`
 * gates the document write and the media-query subscription so a fallback
 * hook mounted next to a provider cannot fight it for <html>.
 */
export function useThemeState(track: boolean): ThemeState {
  const [preference, setPreferenceState] = useState<ThemePreference>(readStoredTheme)
  const [systemDark, setSystemDark] = useState<boolean>(prefersDark)
  const resolved = resolveTheme(preference, systemDark)

  // oxlint-disable react/set-state-in-effect
  useEffect(() => {
    if (track) applyResolvedTheme(resolved)
  }, [track, resolved])
  // oxlint-enable react/set-state-in-effect

  useEffect(() => {
    if (!track) return
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return
    const query = window.matchMedia('(prefers-color-scheme: dark)')
    const onChange = (event: MediaQueryListEvent) => setSystemDark(event.matches)
    query.addEventListener('change', onChange)
    return () => query.removeEventListener('change', onChange)
  }, [track])

  const setPreference = useCallback((next: ThemePreference) => {
    storeTheme(next)
    setPreferenceState(next)
  }, [])

  return { preference, resolved, setPreference }
}

/**
 * Leaf components rendered without the shell (e.g. a page mounted directly in
 * a test) still get a working theme hook instead of throwing.
 */
export function useTheme(): ThemeState {
  const context = useContext(ThemeContext)
  const standalone = useThemeState(context === null)
  return context ?? standalone
}
