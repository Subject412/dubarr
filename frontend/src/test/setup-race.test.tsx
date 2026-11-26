import { Route, Routes } from 'react-router-dom'
import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { SetupPage } from '../pages/Setup'
import { isMethod, jsonResponse, renderWithProviders } from '../test/helpers'

type StatusBody = { initialized: boolean; authenticated: boolean; keyring_available: boolean }

const VIRGIN: StatusBody = { initialized: false, authenticated: false, keyring_available: true }
const CLAIMED: StatusBody = { initialized: true, authenticated: true, keyring_available: true }

/**
 * Mocks the first-run API and dispatches `GET /api/auth/status` on the call
 * COUNT, never on elapsed time. The ordering of the three status calls after
 * submitting the admin form is deterministic:
 *
 *   #1  initial mount query          -> virgin, immediate
 *   #2  invalidated refetch (macrotask, right after the mutation settles)
 *                                    -> claimed, immediate (resolves first)
 *   #3  the wizard's own probe       -> claimed, after `probeDelayMs` (resolves last)
 *
 * Because #2 resolves immediately and #3 is held back, #2 provably commits
 * while the wizard's `await api.authStatus()` is still pending. On the unsafe
 * ordering (`setAdminCreated(true)` after the probe) that commit lands while
 * `adminCreated` is still false, so the guard at Setup.tsx:64 fires and the
 * wizard is unmounted into /library.
 */
function mockFirstRun(
  probeDelayMs: number,
  { probeRejects = false }: { probeRejects?: boolean } = {},
): { statusCalls: () => number } {
  let statusCalls = 0
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/api/auth/setup') && isMethod(init, 'POST')) {
      return jsonResponse({ id: 1, username: 'admin', is_admin: true }, 201)
    }
    if (url.includes('/api/auth/status')) {
      statusCalls += 1
      if (statusCalls === 1) return jsonResponse(VIRGIN)
      if (statusCalls === 2) return jsonResponse(CLAIMED)
      // #3 and anything later: the wizard's probe, deliberately slow so the
      // invalidated refetch has already committed when it resolves.
      await new Promise((resolve) => setTimeout(resolve, probeDelayMs))
      if (probeRejects) throw new TypeError('Failed to fetch')
      return jsonResponse(CLAIMED)
    }
    if (url.includes('/api/auth/login') && isMethod(init, 'POST')) {
      return jsonResponse({ id: 1, username: 'admin', is_admin: true })
    }
    if (url.includes('/api/settings')) {
      return jsonResponse({ settings: {} })
    }
    return jsonResponse({ error: 'unexpected' }, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
  return { statusCalls: () => statusCalls }
}

/** The wizard must be mounted through a real route table so a redirect is observable. */
function renderWizard() {
  return renderWithProviders(
    <Routes>
      <Route path="/setup" element={<SetupPage />} />
      <Route path="/library" element={<div>library-page</div>} />
    </Routes>,
    { route: '/setup' },
  )
}

async function submitAdmin() {
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Username'), 'admin')
  await user.type(screen.getByLabelText('Password'), 'longenough')
  await user.type(screen.getByLabelText('Confirm password'), 'longenough')
  await user.click(screen.getByRole('button', { name: /create administrator/i }))
}

const flush = () => new Promise((resolve) => setTimeout(resolve, 0))

describe('first-run wizard admin race', () => {
  it('does not bounce to /library when the invalidated refetch lands before the probe settles', async () => {
    const mock = mockFirstRun(120)
    renderWizard()
    await submitAdmin()

    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()

    // #2 has committed by now (immediate) and #3 resolved last (delayed); the
    // guard must have stayed quiet throughout. Re-check after a flush and again
    // once the delayed probe has fully settled.
    expect(screen.queryByText('library-page')).not.toBeInTheDocument()
    await flush()
    expect(screen.queryByText('library-page')).not.toBeInTheDocument()
    await new Promise((resolve) => setTimeout(resolve, 200))
    expect(screen.queryByText('library-page')).not.toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()

    // The probe really ran, so the ordering above is the one under test.
    expect(mock.statusCalls()).toBe(3)
  })

  it('still advances to storage when the status probe answers immediately', async () => {
    const mock = mockFirstRun(0)
    renderWizard()
    await submitAdmin()

    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()
    await flush()
    expect(screen.queryByText('library-page')).not.toBeInTheDocument()
    expect(mock.statusCalls()).toBeGreaterThanOrEqual(2)
  })

  it('still advances to storage when the post-setup session probe rejects', async () => {
    // The setup POST succeeds server-side; only the best-effort session probe
    // (#3) fails. The wizard must not be stranded on a form whose account now
    // already exists.
    mockFirstRun(0, { probeRejects: true })
    renderWizard()
    await submitAdmin()

    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()
    await flush()
    expect(screen.queryByText('library-page')).not.toBeInTheDocument()
    expect(screen.queryByText(/could not create the administrator/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/setup already completed/i)).not.toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()
  })
})
