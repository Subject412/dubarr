import { screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { SystemPage } from '../pages/System'
import { jsonResponse, renderWithProviders } from '../test/helpers'

describe('system health page', () => {
  it('renders tool availability and flags a missing mkvmerge', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        jsonResponse({
          status: 'degraded',
          version: '0.1.0-test',
          db_ok: true,
          keyring_available: false,
          secrets_initialized: true,
          auth_initialized: true,
          tools: {
            ffmpeg: { available: true, version: '9.0', path: '/usr/bin/ffmpeg' },
            ffprobe: { available: true, version: '9.0', path: '/usr/bin/ffprobe' },
            mkvmerge: { available: false },
          },
        }),
      ),
    )

    renderWithProviders(<SystemPage />, { route: '/system' })

    expect(await screen.findByText('degraded')).toBeInTheDocument()
    expect(screen.getByText('0.1.0-test')).toBeInTheDocument()

    const mkvmerge = await screen.findByTestId('tool-mkvmerge')
    expect(within(mkvmerge).getByText('not available')).toBeInTheDocument()
    expect(within(mkvmerge).getByText('not built yet')).toBeInTheDocument()

    const ffmpeg = screen.getByTestId('tool-ffmpeg')
    expect(within(ffmpeg).getByText('available')).toBeInTheDocument()
    expect(within(ffmpeg).getByText('version 9.0')).toBeInTheDocument()
    expect(within(ffmpeg).getByText('/usr/bin/ffmpeg')).toBeInTheDocument()
  })
})
