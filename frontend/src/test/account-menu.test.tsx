import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import App from '../App'
import { isMethod, jsonResponse, renderWithProviders } from './helpers'

/**
 * API mock for the authenticated shell. Overrides are matched against the
 * request URL, so a test only spells out the endpoints it cares about.
 */
function mockApi(
  overrides: Record<string, (init?: RequestInit) => Response | Promise<Response>> = {},
) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/api/auth/status')) {
      return jsonResponse({ initialized: true, authenticated: true, keyring_available: true })
    }
    if (url.includes('/api/auth/me')) {
      return jsonResponse({ id: 1, username: 'admin', is_admin: true })
    }
    for (const [fragment, handler] of Object.entries(overrides)) {
      if (url.includes(fragment)) return handler(init)
    }
    return jsonResponse({ error: 'unexpected' }, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

/** Renders the shell and opens the account menu from the username button. */
async function openAccountMenu(user: ReturnType<typeof userEvent.setup>) {
  await screen.findByRole('heading', { name: 'Library' })
  const trigger = await screen.findByRole('button', { name: 'admin' })
  await user.click(trigger)
  return trigger
}

function passwordPosts(fetchMock: ReturnType<typeof mockApi>) {
  return fetchMock.mock.calls.filter(
    ([input, init]) => String(input).includes('/api/auth/password') && isMethod(init, 'POST'),
  )
}

describe('account menu (issue #6)', () => {
  it('opens from the username button and closes on Escape and outside click', async () => {
    const user = userEvent.setup()
    mockApi()
    renderWithProviders(<App />, { route: '/library' })

    const trigger = await openAccountMenu(user)
    expect(trigger).toHaveAttribute('aria-haspopup', 'menu')
    expect(trigger).toHaveAttribute('aria-expanded', 'true')

    const menu = screen.getByRole('menu')
    expect(within(menu).getByRole('menuitem', { name: 'Change password' })).toBeInTheDocument()
    expect(within(menu).getByRole('menuitem', { name: /log out/i })).toBeInTheDocument()

    await user.keyboard('{Escape}')
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
    expect(trigger).toHaveAttribute('aria-expanded', 'false')

    await user.click(trigger)
    expect(screen.getByRole('menu')).toBeInTheDocument()

    await user.click(screen.getByRole('heading', { name: 'Dubarr' }))
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
  })

  it('validates the change-password form before calling the API', async () => {
    const user = userEvent.setup()
    const fetchMock = mockApi()
    renderWithProviders(<App />, { route: '/library' })

    await openAccountMenu(user)
    await user.click(screen.getByRole('menuitem', { name: 'Change password' }))

    const dialog = screen.getByRole('dialog', { name: /change password/i })
    const current = within(dialog).getByLabelText('Current password')
    const next = within(dialog).getByLabelText('New password')
    const confirm = within(dialog).getByLabelText('Confirm new password')
    const submit = within(dialog).getByRole('button', { name: 'Change password' })

    await user.type(current, 'correct horse battery')
    await user.type(next, 'short')
    await user.type(confirm, 'short')
    await user.click(submit)
    expect(within(dialog).getByText(/at least 8/i)).toBeInTheDocument()
    expect(passwordPosts(fetchMock)).toHaveLength(0)

    await user.clear(next)
    await user.type(next, 'long enough password')
    await user.clear(confirm)
    await user.type(confirm, 'long enough passwerd')
    await user.click(submit)
    expect(within(dialog).getByText(/do not match/i)).toBeInTheDocument()
    expect(passwordPosts(fetchMock)).toHaveLength(0)
  })

  it('submits the new password and reports success', async () => {
    const user = userEvent.setup()
    const fetchMock = mockApi({
      '/api/auth/password': () => new Response(null, { status: 204 }),
    })
    renderWithProviders(<App />, { route: '/library' })

    await openAccountMenu(user)
    await user.click(screen.getByRole('menuitem', { name: 'Change password' }))

    const dialog = screen.getByRole('dialog', { name: /change password/i })
    await user.type(within(dialog).getByLabelText('Current password'), 'correct horse battery')
    await user.type(within(dialog).getByLabelText('New password'), 'brand new password')
    await user.type(within(dialog).getByLabelText('Confirm new password'), 'brand new password')
    await user.click(within(dialog).getByRole('button', { name: 'Change password' }))

    await waitFor(() => expect(passwordPosts(fetchMock)).toHaveLength(1))
    const init = passwordPosts(fetchMock)[0][1]
    expect(JSON.parse(String(init?.body))).toEqual({
      current_password: 'correct horse battery',
      new_password: 'brand new password',
    })
    expect(await within(dialog).findByText(/password changed/i)).toBeInTheDocument()
    // The session stays valid, so no redirect: the shell is still mounted.
    expect(screen.getByRole('button', { name: 'admin' })).toBeInTheDocument()
  })

  it('surfaces a rejected current password without logging the user out', async () => {
    const user = userEvent.setup()
    mockApi({
      '/api/auth/password': () => jsonResponse({ error: 'invalid current password' }, 403),
    })
    renderWithProviders(<App />, { route: '/library' })

    await openAccountMenu(user)
    await user.click(screen.getByRole('menuitem', { name: 'Change password' }))

    const dialog = screen.getByRole('dialog', { name: /change password/i })
    await user.type(within(dialog).getByLabelText('Current password'), 'wrong password')
    await user.type(within(dialog).getByLabelText('New password'), 'brand new password')
    await user.type(within(dialog).getByLabelText('Confirm new password'), 'brand new password')
    await user.click(within(dialog).getByRole('button', { name: 'Change password' }))

    expect(await within(dialog).findByText(/invalid current password/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'admin' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: /sign in/i })).not.toBeInTheDocument()
  })
})
