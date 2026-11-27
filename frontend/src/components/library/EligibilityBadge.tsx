import { Badge } from '../ui'
import { eligibilityLabel, eligibilityTone } from '../../lib/library'

/**
 * One Processing Gate verdict, shared by the Films and TV shows tabs so a title
 * reads the same wherever it appears (#56). The three terms are the queue
 * page's — eligible / waiting for tags / missing target — and the labels and
 * tones come from the same helpers the flat library already uses, so there is
 * one vocabulary rather than three.
 *
 * A row the server sent without a verdict renders a dash: the badge never
 * guesses a state for a title whose eligibility was not reported.
 */
export function EligibilityBadge({ status }: { status?: string }) {
  if (!status) {
    return (
      <span className="text-xs text-zinc-500" title="No eligibility verdict reported for this title">
        —
      </span>
    )
  }
  return <Badge tone={eligibilityTone(status)}>{eligibilityLabel(status)}</Badge>
}
