/** Stable id used to wire a control's aria-describedby to its help popover. */
export function helpIdFor(label: string): string {
  return `help-${label
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/(^-|-$)/g, '')}`
}
