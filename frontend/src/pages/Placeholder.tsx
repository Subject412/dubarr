import { Card } from '../components/ui'

export function PlaceholderPage({
  title,
  milestone,
  description,
}: {
  title: string
  milestone: string
  description: string
}) {
  return (
    <div>
      <h1 className="text-2xl font-semibold tracking-tight text-zinc-100">{title}</h1>
      <p className="mt-1 text-sm text-zinc-400">{description}</p>
      <Card className="mt-6">
        <div className="flex flex-col items-center gap-2 py-10 text-center">
          <span className="text-2xl" aria-hidden>
            🚧
          </span>
          <p className="text-sm font-medium text-zinc-200">
            {title} — coming in {milestone}
          </p>
          <p className="max-w-md text-sm text-zinc-500">
            The shell, routing and settings plumbing are live. This workspace arrives in {milestone}.
          </p>
        </div>
      </Card>
    </div>
  )
}
