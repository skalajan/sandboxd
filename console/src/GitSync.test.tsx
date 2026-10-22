import { describe, it, expect, vi } from 'vitest'
import { render, fireEvent, screen, waitFor } from '@testing-library/react'
import { GitTab } from './AppView'
import { gitStatusFixture, gitDiffFixture, installFetch } from './test/fixtures'
import type { GitStatus } from './api'

const noop = () => {}

function setup(status: Partial<GitStatus>, extra: (m: string, p: string) => unknown = () => undefined) {
  const toast = vi.fn()
  const onError = vi.fn()
  installFetch((m, p) => {
    const e = extra(m, p)
    if (e !== undefined) return e
    if (/\/git\/status/.test(p)) return { ...gitStatusFixture, ...status }
    if (/\/git\/diff/.test(p)) return gitDiffFixture
    return undefined
  })
  render(<GitTab appId="app1" sbId="sb1" onError={onError} toast={toast} goSettings={noop} onViewTask={noop} />)
  return { toast, onError }
}

describe('GitTab sync buttons', () => {
  it('enables Rebase and Publish when idle', async () => {
    setup({})
    const publish = (await screen.findByTestId('git-publish')) as HTMLButtonElement
    expect(publish.disabled).toBe(false)
    expect((screen.getByTestId('git-rebase') as HTMLButtonElement).disabled).toBe(false)
    expect(publish.title).toContain('Commits all changed files first')
  })

  it('disables both while a sync is in progress', async () => {
    setup({ in_progress: 'merge', conflicts: ['a.ts'], resolve_task_id: 't1' }, (_m, p) => (/\/tasks\/t1/.test(p) ? { id: 't1', status: 'running' } : undefined))
    await screen.findByTestId('git-resolving')
    expect((screen.getByTestId('git-publish') as HTMLButtonElement).disabled).toBe(true)
    expect((screen.getByTestId('git-rebase') as HTMLButtonElement).disabled).toBe(true)
  })

  it.each([
    [{ state: 'published', commits: 3 }, 'toast', 'Published 3 commits → main'],
    [{ state: 'rebased', commits: 1 }, 'toast', 'Rebased onto main (1 local commit)'],
    [{ state: 'up_to_date' }, 'toast', 'Already up to date'],
    [{ state: 'failed', reason: 'non_fast_forward' }, 'error', 'main moved — click Publish again'],
    [{ state: 'failed', reason: 'no_credential' }, 'error', 'No Git credential — add one in Settings → Git credentials.'],
  ])('maps %j to the right message', async (resp, kind, text) => {
    const { toast, onError } = setup({}, (m, p) => (m === 'POST' && /git\/sync$/.test(p) ? resp : undefined))
    fireEvent.click(await screen.findByTestId('git-publish'))
    await waitFor(() => expect((kind === 'toast' ? toast : onError)).toHaveBeenCalledWith(text))
  })
})

describe('GitTab sync banners', () => {
  it('shows the resolving banner with conflicts and no abort while the task runs', async () => {
    setup({ in_progress: 'rebase', conflicts: ['src/a.ts', 'b.md'], resolve_task_id: 't1' }, (_m, p) => (/\/tasks\/t1/.test(p) ? { id: 't1', status: 'running' } : undefined))
    const b = await screen.findByTestId('git-resolving')
    expect(b.textContent).toContain('Agent resolving 2 conflicts (rebase)')
    expect(b.textContent).toContain('src/a.ts')
    expect(b.getAttribute('data-failed')).toBe('false')
    expect(screen.queryByTestId('git-abort')).toBeNull()
    expect(screen.getByTestId('git-task-link')).toBeTruthy()
  })

  it('offers Abort when the resolve task failed', async () => {
    let aborted = false
    setup({ in_progress: 'merge', conflicts: ['a.ts'], resolve_task_id: 't1' }, (m, p) => {
      if (/\/tasks\/t1/.test(p)) return { id: 't1', status: 'failed' }
      if (m === 'POST' && /sync\/abort$/.test(p)) { aborted = true; return { aborted: true } }
      return undefined
    })
    const abort = await screen.findByTestId('git-abort')
    expect(screen.getByTestId('git-resolving').textContent).toContain("Agent couldn't resolve")
    fireEvent.click(abort)
    await waitFor(() => expect(aborted).toBe(true))
  })

  it('shows the review banner after an agent-resolved merge and loads the range diff', async () => {
    let ranged = false
    setup({ in_progress: '', resolve_task_id: 't1', ahead: 2 }, (_m, p) => {
      if (/git\/diff\?range=/.test(p)) { ranged = true; return { available: true, diff: 'diff --git a/x b/x\n+merged\n' } }
      return undefined
    })
    await screen.findByTestId('git-review')
    fireEvent.click(screen.getByTestId('git-review-toggle'))
    await screen.findByText('origin/main..HEAD — what Publish will push')
    await screen.findByText('+merged')
    expect(ranged).toBe(true)
  })

  it('shows no review banner when nothing is ahead', async () => {
    setup({ in_progress: '', resolve_task_id: 't1', ahead: 0 })
    await screen.findByTestId('git-publish')
    expect(screen.queryByTestId('git-review')).toBeNull()
  })
})
