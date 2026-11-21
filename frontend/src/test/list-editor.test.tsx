import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { ListEditor } from '../components/ui'

function renderEditor(values: number[], onChange = vi.fn()) {
  const view = render(<ListEditor label="Checkpoints" values={values} onChange={onChange} />)
  return {
    ...view,
    input: screen.getByLabelText('Checkpoints') as HTMLInputElement,
    onChange,
    rerenderValues: (next: number[]) =>
      view.rerender(<ListEditor label="Checkpoints" values={next} onChange={onChange} />),
  }
}

describe('ListEditor draft handling', () => {
  it('keeps a dirty draft when a refetch lands while the input is focused', async () => {
    const user = userEvent.setup()
    const { input, rerenderValues } = renderEditor([10, 20])

    await user.click(input)
    await user.clear(input)
    await user.type(input, '33')

    rerenderValues([50, 60])

    expect(input).toHaveValue('33')
  })

  it('does not commit a stale draft over fresh server data on blur', async () => {
    const user = userEvent.setup()
    const { input, onChange, rerenderValues } = renderEditor([10, 20])

    await user.click(input)
    // No local edit: the draft still equals the value captured at focus time.
    rerenderValues([50, 60])

    await user.tab()

    expect(onChange).not.toHaveBeenCalled()
    expect(input).toHaveValue('50, 60')
  })

  it('commits an actually edited draft on blur', async () => {
    const user = userEvent.setup()
    const { input, onChange, rerenderValues } = renderEditor([10, 20])

    await user.click(input)
    await user.clear(input)
    await user.type(input, '33')
    rerenderValues([50, 60])

    await user.tab()

    expect(onChange).toHaveBeenCalledWith([33])
  })
})
