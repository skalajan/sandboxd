import { describe, it, expect, vi } from 'vitest'
import { render, fireEvent, screen } from '@testing-library/react'
import { VersionPill } from './VersionPill'
import { settingsFixture } from './test/fixtures'
import type { Settings } from './api'

const notes = "## What's Changed\n* Faster builds\n\n## Breaking changes\n* `SANDBOXD_PORT` renamed\n"
const withUpdate = (extra: Partial<Settings> = {}): Settings => ({
  ...settingsFixture, version: 'v0.4.0', git_commit: 'abc1234def',
  update_available: true, update_kind: 'release', latest_version: 'v0.5.0',
  changelog_url: 'https://github.com/tastyeffectco/sandboxd/releases/tag/v0.5.0',
  latest_notes: notes, latest_breaking: '* `SANDBOXD_PORT` renamed', latest_published_at: '2026-08-01T00:00:00Z', ...extra,
})
const noop = () => {}

describe('VersionPill', () => {
  it('shows the running version and no dot when up to date', () => {
    render(<VersionPill settings={settingsFixture} dismissed={false} onDismiss={noop} upg={null} startUpgrade={noop} clearUpgrade={noop} />)
    const pill = screen.getByTestId('version-pill')
    expect(pill.textContent).toBe('DevBox v0.4.0')
    expect(pill.getAttribute('data-version')).toBe('v0.4.0')
    expect(screen.queryByTestId('version-dot')).toBeNull()
    fireEvent.click(pill)
    expect(screen.getByTestId('version-panel').textContent).toContain('latest release')
    expect(screen.queryByTestId('upgrade-open')).toBeNull()
  })

  it('announces a release update and renders notes, links and the issue template', () => {
    render(<VersionPill settings={withUpdate()} dismissed={false} onDismiss={noop} upg={null} startUpgrade={noop} clearUpgrade={noop} />)
    expect(screen.getByTestId('version-pill').textContent).toBe('DevBox v0.4.0 · v0.5.0 available')
    expect(screen.getByTestId('version-dot')).toBeTruthy()
    fireEvent.click(screen.getByTestId('version-pill'))
    const panel = screen.getByTestId('version-panel')
    expect(panel.textContent).toContain('v0.4.0 · abc1234')
    expect(panel.textContent).toContain("What's new in v0.5.0 · 2026-08-01")
    expect(screen.getByTestId('update-notes').textContent).toContain('Faster builds')
    expect(screen.getByTestId('breaking-box').textContent).toContain('SANDBOXD_PORT')
    const issue = screen.getByTestId('report-issue').getAttribute('href') || ''
    expect(issue.startsWith('https://github.com/skalajan/sandboxd/issues/new?title=&body=')).toBe(true)
    const body = decodeURIComponent(issue.split('body=')[1])
    expect(body).toContain('**Version:** v0.4.0 (abc1234)')
    expect(body).toContain('**Preview host style:** nested')
    expect(body).toContain('**Browser:** ')
    expect(body).not.toContain('localhost')
  })

  it('requires the breaking-changes acknowledgement before upgrading', () => {
    const start = vi.fn()
    render(<VersionPill settings={withUpdate()} dismissed={false} onDismiss={noop} upg={null} startUpgrade={start} clearUpgrade={noop} />)
    fireEvent.click(screen.getByTestId('version-pill'))
    fireEvent.click(screen.getByTestId('upgrade-open'))
    expect(screen.getByTestId('upgrade-confirm')).toBeTruthy()
    const go = screen.getByTestId('upgrade-go') as HTMLButtonElement
    expect(go.disabled).toBe(true)
    fireEvent.click(go)
    expect(start).not.toHaveBeenCalled()
    fireEvent.click(screen.getByTestId('upgrade-ack'))
    expect(go.disabled).toBe(false)
    fireEvent.click(go)
    expect(start).toHaveBeenCalledWith('v0.5.0')
  })

  it('upgrades straight away when nothing is breaking', () => {
    const start = vi.fn()
    render(<VersionPill settings={withUpdate({ latest_notes: '* x', latest_breaking: undefined })} dismissed={false} onDismiss={noop} upg={null} startUpgrade={start} clearUpgrade={noop} />)
    fireEvent.click(screen.getByTestId('version-pill'))
    expect(screen.queryByTestId('breaking-box')).toBeNull()
    fireEvent.click(screen.getByTestId('upgrade-open'))
    expect(screen.queryByTestId('upgrade-ack')).toBeNull()
    fireEvent.click(screen.getByTestId('upgrade-go'))
    expect(start).toHaveBeenCalledWith('v0.5.0')
  })

  it('words untagged builds softly', () => {
    render(<VersionPill settings={withUpdate({ version: 'e2ca6f6', update_kind: 'untagged' })} dismissed={false} onDismiss={noop} upg={null} startUpgrade={noop} clearUpgrade={noop} />)
    expect(screen.getByTestId('version-pill').textContent).toBe('DevBox e2ca6f6 · untagged build · latest v0.5.0')
    fireEvent.click(screen.getByTestId('version-pill'))
    expect(screen.getByTestId('version-panel').textContent).toContain("You're on an untagged build (e2ca6f6); the latest release is v0.5.0")
  })

  it('dismissal drops the dot and the reminder link but keeps the update in the panel', () => {
    const dismiss = vi.fn()
    const { rerender } = render(<VersionPill settings={withUpdate()} dismissed={false} onDismiss={dismiss} upg={null} startUpgrade={noop} clearUpgrade={noop} />)
    fireEvent.click(screen.getByTestId('version-pill'))
    fireEvent.click(screen.getByTestId('update-dismiss'))
    expect(dismiss).toHaveBeenCalled()
    rerender(<VersionPill settings={withUpdate()} dismissed={true} onDismiss={dismiss} upg={null} startUpgrade={noop} clearUpgrade={noop} />)
    expect(screen.queryByTestId('version-dot')).toBeNull()
    expect(screen.queryByTestId('update-dismiss')).toBeNull()
    expect(screen.getByTestId('upgrade-open')).toBeTruthy()
  })

  it('shows upgrade progress and failure in the pill', () => {
    const clear = vi.fn()
    const { rerender } = render(<VersionPill settings={withUpdate()} dismissed={false} onDismiss={noop} upg={{ phase: 'running', target: 'v0.5.0' }} startUpgrade={noop} clearUpgrade={clear} />)
    expect(screen.getByTestId('version-pill').textContent).toBe('Upgrading to v0.5.0…')
    fireEvent.click(screen.getByTestId('version-pill'))
    expect(screen.getByTestId('upgrade-progress')).toBeTruthy()
    rerender(<VersionPill settings={withUpdate()} dismissed={false} onDismiss={noop} upg={{ phase: 'rolled_back', target: 'v0.5.0', message: 'health check failed' }} startUpgrade={noop} clearUpgrade={clear} />)
    expect(screen.getByTestId('version-pill').textContent).toBe('Upgrade rolled back')
    expect(screen.getByTestId('upgrade-result').textContent).toContain('health check failed')
    fireEvent.click(screen.getByText('ok'))
    expect(clear).toHaveBeenCalled()
  })

  it('closes on Escape', () => {
    render(<VersionPill settings={withUpdate()} dismissed={false} onDismiss={noop} upg={null} startUpgrade={noop} clearUpgrade={noop} />)
    fireEvent.click(screen.getByTestId('version-pill'))
    expect(screen.getByTestId('version-panel')).toBeTruthy()
    fireEvent.keyDown(window, { key: 'Escape' })
    expect(screen.queryByTestId('version-panel')).toBeNull()
  })
})
