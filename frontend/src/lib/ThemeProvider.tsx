import type { ReactNode } from 'react'
import { ThemeContext, useThemeState } from './theme'

/** Applies the stored theme and keeps `system` in sync with the OS. */
export function ThemeProvider({ children }: { children: ReactNode }) {
  const value = useThemeState(true)
  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>
}
