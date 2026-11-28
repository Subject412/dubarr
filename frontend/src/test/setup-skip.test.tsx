import { Route, Routes } from 'react-router-dom'
import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { SetupPage } from '../pages/Setup'
import { isMethod, jsonResponse, renderWithProviders } from '../test/helpers'

type StatusBody = { initialized: boolean; authenticated: boolean; keyring_available: boolean }

const VIRGIN: StatusBody = { initialized: false, authenticated: false, keyring_available: true }
const CLAIMED: StatusBody = { initialized: true, authenticated: true, keyring_available: true }

/**
 * First-run mock: `GET /api/auth/status` answers virgin on the first call and
 * claimed afterwards (the invalidated refetch and the wizard's own probe), so
 * the wizard advances to Storage exactly like the real appliance. Settings
 * reads return an empty document, which keeps the local storage paths at their
 * documented empty defaults.
 */
function mockFirstRun(): ReturnType<typeof vi.fn> {
  let statusCalls = 0
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/api/auth/setup') && isMethod(init, 'POST')) {
      return jsonResponse({ id: 1, username: 'admin', is_admin: true }, 201)
    }
    if (url.includes('/api/auth/status')) {
      statusCalls += 1
      return jsonResponse(statusCalls === 1 ? VIRGIN : CLAIMED)
    }
    if (url.includes('/api/auth/login') && isMethod(init, 'POST')) {
      return jsonResponse({ id: 1, username: 'admin', is_admin: true })
    }
    if (url.includes('/api/settings')) {
      return jsonResponse({ settings: {} })
    }
    return jsonResponse({ error: `unexpected ${url}` }, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

/** The wizard must be mounted through a real route table so navigation is observable. */
function renderWizard() {
  return renderWithProviders(
    <Routes>
      <Route path="/setup" element={<SetupPage />} />
      <Route path="/library" element={<div>library-page</div>} />
    </Routes>,
    { route: '/setup' },
  )
}

async function createAdminAndReachStorage() {
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Username'), 'admin')
  await user.type(screen.getByLabelText('Password'), 'longenough')
  await user.type(screen.getByLabelText('Confirm password'), 'longenough')
  await user.click(screen.getByRole('button', { name: /create administrator/i }))
  expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()
  return user
}

const skipButton = () => screen.queryByRole('button', { name: 'Set up later' })

/** Walks the rest of the wizard without persisting anything meaningful. */
async function reachFinishStep(user: ReturnType<typeof userEvent.setup>) {
  await user.type(screen.getByLabelText('Watch folder'), '/media/watch')
  await user.type(screen.getByLabelText('Audio Vault'), '/media/vault')
  await user.click(screen.getByRole('button', { name: /save & continue/i }))
  expect(await screen.findByRole('heading', { name: 'Languages & tags' })).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: /save & continue/i }))
  expect(await screen.findByRole('heading', { name: 'Radarr & Sonarr' })).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Skip for now' }))
  // The *arr step is the last one before Finish: its Next lands there directly.
  expect(await screen.findByRole('heading', { name: 'Ready to go' })).toBeInTheDocument()
  expect(screen.queryByRole('heading', { name: 'Security' })).toBeNull()
}

function mutationCalls(fetchMock: ReturnType<typeof vi.fn>) {
  return fetchMock.mock.calls.filter(([, init]) =>
    ['PUT', 'POST', 'PATCH', 'DELETE'].includes(
      ((init as RequestInit | undefined)?.method ?? 'GET').toUpperCase(),
    ),
  )
}

describe('first-run wizard escape hatch', () => {
  it('does not offer the skip on the administrator step', () => {
    mockFirstRun()
    renderWizard()

    expect(screen.getByRole('heading', { name: 'Administrator account' })).toBeInTheDocument()
    expect(skipButton()).toBeNull()

    // The wizard header carries the same decorative mark, so its single
    // "Dubarr" heading is still the only one announced.
    const mark = document.querySelector('img[src="/icon-512.png"]')
    expect(mark?.getAttribute('aria-hidden')).toBe('true')
    expect(mark?.getAttribute('alt')).toBe('')
    expect(screen.getAllByRole('heading', { name: /Dubarr/ })).toHaveLength(1)
  })

  it('offers the skip after the administrator is created', async () => {
    mockFirstRun()
    renderWizard()
    await createAdminAndReachStorage()

    expect(skipButton()).not.toBeNull()
  })

  it('skipping enters the app without saving anything', async () => {
    const fetchMock = mockFirstRun()
    renderWizard()
    const user = await createAdminAndReachStorage()

    // Only the administrator POST has happened so far.
    const before = mutationCalls(fetchMock).length
    expect(before).toBe(1)

    // The primary action is untouched: the skip is an extra ghost button, not a
    // replacement, and it is not a form submit.
    const skip = screen.getByRole('button', { name: 'Set up later' })
    expect(screen.getByRole('button', { name: /save & continue/i })).not.toBeNull()
    expect(skip.getAttribute('type')).not.toBe('submit')

    await user.click(skip)

    expect(await screen.findByText('library-page')).toBeInTheDocument()

    // Leaving must not persist anything: no further mutating requests at all.
    expect(mutationCalls(fetchMock).length).toBe(before)
    expect(
      fetchMock.mock.calls.filter(
        ([input, init]) => String(input).includes('/api/settings') && isMethod(init, 'PUT'),
      ),
    ).toHaveLength(0)
  })

  it('warns about unset folders', async () => {
    mockFirstRun()
    renderWizard()
    await createAdminAndReachStorage()

    // The warning is gated on the settings query having settled, so wait for it
    // rather than reading synchronously.
    const warning = await screen.findByText(/still unset/i)
    expect(warning).toHaveTextContent(/watch folder/i)
    expect(warning).toHaveTextContent(/vault folder/i)
  })

  it('does not offer the skip on the finish step', async () => {
    mockFirstRun()
    renderWizard()
    const user = await createAdminAndReachStorage()
    await reachFinishStep(user)

    expect(screen.getByRole('button', { name: 'Open Library' })).not.toBeNull()
    expect(skipButton()).toBeNull()
  })

  it('lists five steps and never announces a Security step', () => {
    mockFirstRun()
    renderWizard()

    const progress = screen.getByRole('list', { name: 'Setup progress' })
    const labels = within(progress)
      .getAllByRole('listitem')
      .map((item) => item.lastElementChild?.textContent)
    expect(labels).toEqual([
      'Administrator',
      'Storage',
      'Languages & tags',
      '*arr connections',
      'Finish',
    ])
    expect(screen.queryByRole('heading', { name: 'Security' })).toBeNull()
  })

  it('advances from the *arr step straight to Finish', async () => {
    const fetchMock = mockFirstRun()
    renderWizard()
    const user = await createAdminAndReachStorage()

    await user.type(screen.getByLabelText('Watch folder'), '/media/watch')
    await user.type(screen.getByLabelText('Audio Vault'), '/media/vault')
    await user.click(screen.getByRole('button', { name: /save & continue/i }))
    expect(await screen.findByRole('heading', { name: 'Languages & tags' })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /save & continue/i }))
    expect(await screen.findByRole('heading', { name: 'Radarr & Sonarr' })).toBeInTheDocument()

    const before = mutationCalls(fetchMock).length
    await user.click(screen.getByRole('button', { name: 'Continue' }))

    expect(await screen.findByRole('heading', { name: 'Ready to go' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Security' })).toBeNull()
    // Advancing is a pure view change: the arr step does not write the security
    // section, so no additional mutation may appear.
    expect(mutationCalls(fetchMock).length).toBe(before)
  })

  it('reviews the saved answers on Finish and opens the library without writing', async () => {
    const fetchMock = mockFirstRun()
    renderWizard()
    const user = await createAdminAndReachStorage()
    await reachFinishStep(user)

    expect(screen.getByText('/media/watch · /media/vault')).toBeInTheDocument()
    const before = mutationCalls(fetchMock).length
    // POST /api/auth/setup plus the storage and languages PUTs; nothing else.
    expect(before).toBe(3)

    await user.click(screen.getByRole('button', { name: 'Open Library' }))

    expect(await screen.findByText('library-page')).toBeInTheDocument()
    expect(mutationCalls(fetchMock).length).toBe(before)
  })

  it('leaves from the *arr step without a Security step or any write', async () => {
    const fetchMock = mockFirstRun()
    renderWizard()
    const user = await createAdminAndReachStorage()
    await reachFinishStep(user)
    // Walk back to the *arr step: Finish is the last step, so Back must land on
    // the arr connections, not on a removed Security form.
    await user.click(screen.getByRole('button', { name: 'Back' }))
    expect(await screen.findByRole('heading', { name: 'Radarr & Sonarr' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Security' })).toBeNull()

    const before = mutationCalls(fetchMock).length
    await user.click(screen.getByRole('button', { name: 'Set up later' }))

    expect(await screen.findByText('library-page')).toBeInTheDocument()
    expect(mutationCalls(fetchMock).length).toBe(before)
  })
})
