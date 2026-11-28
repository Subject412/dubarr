import type { TargetTagFilter } from './api'
import example from './tag-filter-regex-example.txt?raw'

/**
 * The one pattern the UI shows for REGEX tag mode. It lives in a plain text
 * file so the gate's own test compiles the same bytes with Go's regexp package
 * (RE2) — the hint an operator copies is a pattern the engine accepts.
 */
export const TAG_FILTER_REGEX_EXAMPLE = example.trim()

/**
 * The placeholder and the one-line explanation for a tag list, by match mode.
 * REGEX entries are compiled by the gate with Go's RE2 syntax, so the hint
 * names that dialect instead of letting an operator assume PCRE.
 */
export function tagListHint(mode: TargetTagFilter['tag_match_mode']): {
  placeholder: string
  note: string
} {
  if (mode !== 'REGEX') return { placeholder: '[TAG]', note: '' }
  return {
    placeholder: TAG_FILTER_REGEX_EXAMPLE,
    note: "Each entry is a regular expression in Go's RE2 syntax, matched against the filename (not its folder). Two PCRE features do not compile here: no backreferences (\\1) and no lookaround ((?=…), (?<=…)). Case sensitivity follows the case-sensitive switch.",
  }
}
