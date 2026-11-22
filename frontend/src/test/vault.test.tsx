import { screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { VaultPage } from '../pages/Vault'
import { isMethod, jsonResponse, renderWithProviders } from '../test/helpers'

const items = [
  {
    id: 7,
    title: 'Shrek',
    media_type: 'movie',
    season_number: null,
    episode_number: null,
    filename: 'Shrek.2001.pl.mka',
    file_size: 20 * 1024 * 1024,
    sha256: '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef',
    language: 'pl',
    language_iso3: 'pol',
    variant: 'dubbing',
    codec: 'eac3',
    channels: '5.1',
    channel_count: 6,
    source_release_name: 'Shrek.2001.PL.DUB.mkv',
    created_at: '2026-11-22 10:00:00',
  },
  {
    id: 8,
    title: 'Minions and Monsters',
    media_type: 'series',
    season_number: 1,
    episode_number: 2,
    filename: 'Minions.S01E02.pl.mka',
    file_size: 1024,
    sha256: 'fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210',
    language: 'pl',
    variant: 'dubbing',
    codec: 'ac3',
    channels: '2.0',
    channel_count: 2,
    source_release_name: 'Minions.S01E02.PL.DUB.mkv',
    created_at: '2026-11-22 11:00:00',
  },
]

function vaultFetch(itemsBody: unknown = { items }) {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.endsWith('/api/vault') && isMethod(init, 'GET')) {
      return jsonResponse(itemsBody)
    }
    return jsonResponse({ error: 'unexpected' }, 404)
  })
}

describe('vault page', () => {
  it('renders archived tracks read-only with metadata', async () => {
    vi.stubGlobal('fetch', vaultFetch())
    renderWithProviders(<VaultPage />, { route: '/vault' })

    const first = await screen.findByTestId('vault-item-8')
    expect(within(first).getByText('Minions.S01E02.pl.mka')).toBeInTheDocument()
    expect(within(first).getByText('S01E02')).toBeInTheDocument()
    expect(within(first).getByText('ac3')).toBeInTheDocument()
    expect(within(first).getByText('2.0')).toBeInTheDocument()
    expect(within(first).getByText('1.0 KiB')).toBeInTheDocument()
    expect(within(first).getByText('pl')).toBeInTheDocument()

    const movie = screen.getByTestId('vault-item-7')
    expect(within(movie).getByText('Shrek')).toBeInTheDocument()
    expect(within(movie).getByText('5.1')).toBeInTheDocument()
    expect(within(movie).getByText(/0123456789abcdef/)).toBeInTheDocument()

    // Read-only: no delete/remove controls anywhere on the page.
    expect(screen.queryByRole('button', { name: /delete|remove/i })).toBeNull()
  })

  it('shows a count badge', async () => {
    vi.stubGlobal('fetch', vaultFetch())
    renderWithProviders(<VaultPage />, { route: '/vault' })
    expect(await screen.findByText('2 tracks')).toBeInTheDocument()
  })

  it('explains how the vault fills when empty', async () => {
    vi.stubGlobal('fetch', vaultFetch({ items: [] }))
    renderWithProviders(<VaultPage />, { route: '/vault' })

    expect(await screen.findByText('Vault is empty')).toBeInTheDocument()
    expect(
      screen.getByText(/fills as approvals complete/i),
    ).toBeInTheDocument()
    expect(screen.getByText('0 tracks')).toBeInTheDocument()
  })
})
