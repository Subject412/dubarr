import { cn } from '../lib/cn'
import { THEME_PREFERENCES, useTheme, type ThemePreference } from '../lib/theme'
import { Card } from './ui'

const labels: Record<ThemePreference, { label: string; description: string }> = {
  light: { label: 'Light', description: 'Always use the light palette.' },
  dark: { label: 'Dark', description: 'Always use the dark palette (default).' },
  system: { label: 'System', description: 'Follow the operating-system setting.' },
}

/**
 * Appearance section of Settings. Client-side only: the preference lives in
 * localStorage so it also covers /login and /setup.
 */
export function ThemeSelector() {
  const { preference, resolved, setPreference } = useTheme()

  return (
    <Card
      title="Appearance"
      description="Theme for this browser. Applies immediately, including the sign-in and setup screens."
    >
      <div role="radiogroup" aria-label="Theme" className="grid gap-2 sm:grid-cols-3">
        {THEME_PREFERENCES.map((option) => (
          <label
            key={option}
            className={cn(
              'flex cursor-pointer items-start gap-2 rounded-lg border px-3 py-2 text-sm transition-colors',
              preference === option
                ? 'border-accent-500 bg-accent-500/10 text-zinc-100'
                : 'border-ink-700 bg-ink-950/40 text-zinc-300 hover:border-ink-600 hover:text-zinc-100',
            )}
          >
            <input
              type="radio"
              name="theme"
              value={option}
              aria-label={labels[option].label}
              checked={preference === option}
              onChange={() => setPreference(option)}
              className="mt-0.5 accent-accent-500"
            />
            <span>
              <span className="block font-medium">{labels[option].label}</span>
              <span className="block text-xs text-zinc-500">{labels[option].description}</span>
            </span>
          </label>
        ))}
      </div>
      <p className="mt-3 text-xs text-zinc-500">
        Current theme: <span className="font-medium text-zinc-300">{resolved}</span>.
      </p>
    </Card>
  )
}
