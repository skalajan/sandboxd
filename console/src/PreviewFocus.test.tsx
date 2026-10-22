import { describe, it, expect } from 'vitest'
import { render, fireEvent, screen } from '@testing-library/react'
import { PreviewFocusButton } from './PreviewFocus'

const attr = () => document.documentElement.dataset.previewExpanded

describe('PreviewFocusButton', () => {
  it('toggles the focus attribute and aria-pressed', () => {
    render(<PreviewFocusButton />)
    const btn = screen.getByTestId('preview-expand')
    expect(btn.getAttribute('aria-pressed')).toBe('false')
    expect(attr()).toBeUndefined()
    fireEvent.click(btn)
    expect(btn.getAttribute('aria-pressed')).toBe('true')
    expect(btn.getAttribute('aria-label')).toBe('Exit focus mode')
    expect(attr()).toBe('')
    fireEvent.click(btn)
    expect(btn.getAttribute('aria-pressed')).toBe('false')
    expect(attr()).toBeUndefined()
  })

  it('collapses on Escape', () => {
    render(<PreviewFocusButton />)
    fireEvent.click(screen.getByTestId('preview-expand'))
    fireEvent.keyDown(window, { key: 'Escape' })
    expect(screen.getByTestId('preview-expand').getAttribute('aria-pressed')).toBe('false')
    expect(attr()).toBeUndefined()
  })

  it('ignores Escape while a textarea is focused', () => {
    render(
      <>
        <PreviewFocusButton />
        <textarea data-testid="ta" />
      </>,
    )
    fireEvent.click(screen.getByTestId('preview-expand'))
    screen.getByTestId('ta').focus()
    fireEvent.keyDown(window, { key: 'Escape' })
    expect(screen.getByTestId('preview-expand').getAttribute('aria-pressed')).toBe('true')
    expect(attr()).toBe('')
  })

  it('ignores Escape while a dialog is open', () => {
    render(
      <>
        <PreviewFocusButton />
        <dialog open />
      </>,
    )
    fireEvent.click(screen.getByTestId('preview-expand'))
    fireEvent.keyDown(window, { key: 'Escape' })
    expect(screen.getByTestId('preview-expand').getAttribute('aria-pressed')).toBe('true')
  })

  it('clears the attribute on unmount', () => {
    const { unmount } = render(<PreviewFocusButton />)
    fireEvent.click(screen.getByTestId('preview-expand'))
    expect(attr()).toBe('')
    unmount()
    expect(attr()).toBeUndefined()
  })
})
