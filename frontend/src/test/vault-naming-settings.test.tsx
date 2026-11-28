import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { SettingsPage } from '../pages/Settings'
import { VAULT_TOKEN_LIST } from '../lib/vault-naming'
import { isMethod, jsonResponse, renderWithProviders } from '../test/helpers'

const NAMING = {
  movie_template: '{Title} ({Year}) [{Variant} {Codec} {Channels}]',
  episode_template: '{Title} - S{Season:00}E{Episode:00} - {Episode Title} [{Variant} {Codec} {Channels}]',
  season_folder: 'Season {Season:00}',
}

const SETTINGS = { 'vault.naming': NAMING }

function mockSettings() {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/api/settings') && isMethod(init, 'PUT')) {
      return jsonResponse({ settings: SETTINGS })
    }
    if (url.includes('/api/settings')) {
      return jsonResponse({ settings: SETTINGS })
    }
    return jsonResponse({ error: 'unexpected' }, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

async function renderSettings() {
  const fetchMock = mockSettings()
  renderWithProviders(<SettingsPage />, { route: '/settings' })
  expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()
  return fetchMock
}

describe('vault naming settings (#47)', () => {
  it('shows the two patterns and saves them back unchanged', async () => {
    const user = userEvent.setup()
    const fetchMock = await renderSettings()

    const movie = screen.getByLabelText('Movie file name')
    expect(movie).toHaveValue(NAMING.movie_template)
    expect(screen.getByLabelText('Episode file name')).toHaveValue(NAMING.episode_template)
    expect(screen.getByLabelText('Season folder')).toHaveValue(NAMING.season_folder)
    expect(screen.getByLabelText('Naming style')).toHaveValue('arr')

    fireEvent.change(movie, { target: { value: '{Title} ({Year})' } })
    await user.click(screen.getByLabelText('Save Vault naming'))
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true)
    })

    const put = fetchMock.mock.calls.find(([, init]) => isMethod(init, 'PUT'))
    const body = JSON.parse(String(put?.[1]?.body))
    expect(body.settings['vault.naming']).toEqual({
      movie_template: '{Title} ({Year})',
      episode_template: NAMING.episode_template,
      season_folder: NAMING.season_folder,
    })
  })

  it('renders the live sample and updates it as the pattern changes, without a request', async () => {
    const user = userEvent.setup()
    const fetchMock = await renderSettings()

    expect(screen.getByTestId('vault-naming-preview-movie')).toHaveTextContent(
      'Spirited Away (2001) [Dubbing EAC3 5.1].pl.mka',
    )
    expect(screen.getByTestId('vault-naming-preview-episode')).toHaveTextContent(
      'The Office - S02E03 - Office Olympics [Lektor EAC3 2.0].pl.mka',
    )

    fireEvent.change(screen.getByLabelText('Movie file name'), {
      target: { value: '{Title} ({Year}) [{Variant}]' },
    })

    expect(screen.getByTestId('vault-naming-preview-movie')).toHaveTextContent(
      'Spirited Away (2001) [Dubbing].pl.mka',
    )

    // A preset fills both patterns, and the previews follow at once.
    await user.selectOptions(screen.getByLabelText('Naming style'), 'title_year')
    expect(screen.getByLabelText('Movie file name')).toHaveValue('{Title} ({Year})')
    expect(screen.getByTestId('vault-naming-preview-movie')).toHaveTextContent(
      'Spirited Away (2001).pl.mka',
    )
    expect(screen.getByTestId('vault-naming-preview-episode')).toHaveTextContent(
      'The Office - S02E03.pl.mka',
    )

    // The preview is local: it never asks the server for a sample.
    expect(fetchMock.mock.calls.filter(([, init]) => isMethod(init, 'POST'))).toHaveLength(0)
  })

  it('shows the {Language} refusal against the movie field, not the episode field', async () => {
    await renderSettings()

    fireEvent.change(screen.getByLabelText('Movie file name'), {
      target: { value: '{Title} {Language}' },
    })

    const movie = screen.getByTestId('vault-naming-movie')
    const alert = within(movie).getByRole('alert')
    expect(alert).toHaveTextContent('{Language} is not available')
    expect(alert).toHaveTextContent('.<language>.mka')
    expect(within(screen.getByTestId('vault-naming-episode')).queryByRole('alert')).toBeNull()
  })

  it('refuses a season folder that is a path, against the season folder field', async () => {
    await renderSettings()

    fireEvent.change(screen.getByLabelText('Season folder'), { target: { value: 'Season/02' } })

    expect(within(screen.getByTestId('vault-naming-season')).getByRole('alert')).toHaveTextContent(
      'single folder name',
    )
  })

  it('TestCustomIsNotASelectablePreset: the default patterns offer exactly the two presets', async () => {
    await renderSettings()

    const select = screen.getByLabelText('Naming style') as HTMLSelectElement
    const options = Array.from(select.options)

    expect(options.map((option) => option.textContent)).toEqual([
      'Arr style (recommended)',
      'Title and year only',
    ])
    expect(options.map((option) => option.value)).toEqual(['arr', 'title_year'])
    expect(options.some((option) => option.value === 'custom')).toBe(false)
    expect(select).toHaveValue('arr')
  })

  it('TestEditedPatternsShowCustomAsAStatus: an edited pattern shows a disabled Custom (edited)', async () => {
    const user = userEvent.setup()
    await renderSettings()

    fireEvent.change(screen.getByLabelText('Movie file name'), {
      target: { value: '{Title} ({Year}) [{Variant}]' },
    })

    const select = screen.getByLabelText('Naming style') as HTMLSelectElement
    // The state is displayed, and it is what the control reports as its value.
    expect(select).toHaveValue('custom')
    const status = within(select).getByRole('option', { name: 'Custom (edited)' })
    expect(status).toBeDisabled()
    expect(select.options[select.selectedIndex]).toBe(status)
    // Both real presets are still on offer next to it.
    expect(select.options).toHaveLength(3)

    // A real preset still replaces all three patterns and re-selects itself.
    await user.selectOptions(select, 'title_year')
    expect(screen.getByLabelText('Movie file name')).toHaveValue('{Title} ({Year})')
    expect(screen.getByLabelText('Episode file name')).toHaveValue(
      '{Title} - S{Season:00}E{Episode:00}',
    )
    expect(screen.getByLabelText('Season folder')).toHaveValue('Season {Season:00}')
    expect(select).toHaveValue('title_year')
    expect(within(select).queryByRole('option', { name: 'Custom (edited)' })).toBeNull()
  })

  it('TestTokenHelpListsTheIDs: the token list and help name eleven tokens and the preview renders them', async () => {
    const user = userEvent.setup()
    await renderSettings()

    // The picker's list is the closed set: the three ids last, each once.
    expect(VAULT_TOKEN_LIST.split(', ')).toEqual([
      '{Title}',
      '{Year}',
      '{Season:00}',
      '{Episode:00}',
      '{Episode Title}',
      '{Variant}',
      '{Codec}',
      '{Channels}',
      '{TmdbId}',
      '{TvdbId}',
      '{ImdbId}',
    ])

    const trigger = screen.getByRole('button', { name: 'About Movie file name' })
    await user.hover(trigger)
    const tooltip = await screen.findByRole('tooltip')
    for (const token of ['{TmdbId}', '{TvdbId}', '{ImdbId}']) {
      expect(tooltip).toHaveTextContent(token)
    }
    await user.unhover(trigger)

    // The preview accepts the new tokens and renders the bare id.
    fireEvent.change(screen.getByLabelText('Movie file name'), {
      target: { value: '{Title} ({Year}) [{TmdbId}][{ImdbId}]' },
    })
    expect(screen.getByTestId('vault-naming-preview-movie')).toHaveTextContent(
      'Spirited Away (2001) [129][tt0245429].pl.mka',
    )

    // A film has no TVDB id: the token drops its group, never "[]" or 0.
    fireEvent.change(screen.getByLabelText('Movie file name'), {
      target: { value: '{Title} ({Year}) [{TvdbId}]' },
    })
    expect(screen.getByTestId('vault-naming-preview-movie')).toHaveTextContent(
      'Spirited Away (2001).pl.mka',
    )
    expect(within(screen.getByTestId('vault-naming-movie')).queryByRole('alert')).toBeNull()
  })
})
