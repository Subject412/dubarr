/**
 * Friendly labels for the reserved Reference Language tokens (GLOSSARY).
 *
 * Lives beside the help copy rather than in a page so Settings and the setup
 * wizard offer exactly the same one-click entries: two literal lists would drift
 * the moment one page gains a token.
 */
export const REFERENCE_TOKEN_PRESETS = [
  { value: 'org', label: 'org — original language from *arr' },
  { value: 'all', label: 'all — any shared language' },
] as const
