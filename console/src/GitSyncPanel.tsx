import { useEffect, useState } from 'react'
import type { Dispatch, SetStateAction } from 'react'
import { api } from './api'
import type { GitStatus } from './api'
import { c, mono, Btn } from './design/kit'

export function useGitSync({
  appId,
  sbId,
  st,
  setSt,
  load,
  onError,
  toast,
  setDiffPath,
  setDiff,
  setDiffLoading,
}: {
  appId: string
  sbId?: string
  st: GitStatus | null
  setSt: Dispatch<SetStateAction<GitStatus | null>>
  load: () => Promise<unknown>
  onError: (m: string) => void
  toast: (m: string) => void
  setDiffPath: (p: string | null) => void
  setDiff: (d: string) => void
  setDiffLoading: (b: boolean) => void
}) {
  const [syncing, setSyncing] = useState<'publish' | 'rebase' | null>(null)
  const [resolveFailed, setResolveFailed] = useState(false)
  const [rangeMode, setRangeMode] = useState(false)
  const [reviewDismissed, setReviewDismissed] = useState(false)

  const inProgress = st?.in_progress || ''
  const resolveTaskId = st?.resolve_task_id
  // While an agent resolves conflicts, poll status and the task so the banner survives reloads and flips on failure.
  useEffect(() => {
    if (!inProgress) {
      setResolveFailed(false)
      return
    }
    const tick = async () => {
      try {
        if (sbId && resolveTaskId) {
          const t = await api.getTask(sbId, resolveTaskId)
          setResolveFailed(['failed', 'cancelled', 'error'].includes(t.status))
        }
        setSt(await api.gitStatus(appId))
      } catch {
        // retry on next tick
      }
    }
    void tick()
    const h = setInterval(tick, 3000)
    return () => clearInterval(h)
  }, [inProgress, resolveTaskId, sbId, appId, setSt])

  const sync = async (mode: 'publish' | 'rebase') => {
    setSyncing(mode)
    try {
      const r = await api.gitSync(appId, mode)
      if (r.state === 'published') toast(`Published ${r.commits ?? 0} commit${r.commits === 1 ? '' : 's'} → main`)
      else if (r.state === 'rebased')
        toast(`Rebased onto main (${r.commits ?? 0} local commit${r.commits === 1 ? '' : 's'})`)
      else if (r.state === 'up_to_date') toast('Already up to date')
      else if (r.state === 'resolving')
        toast(`Agent resolving ${r.conflicts?.length ?? 0} conflict${r.conflicts?.length === 1 ? '' : 's'}…`)
      else if (r.reason === 'non_fast_forward') onError('main moved — click Publish again')
      else if (r.reason === 'no_credential') onError('No Git credential — add one in Settings → Git credentials.')
      else onError(r.reason || 'sync failed')
      setResolveFailed(false)
      setReviewDismissed(false)
      setRangeMode(false)
      await load()
    } catch (e) {
      onError((e as Error).message)
    } finally {
      setSyncing(null)
    }
  }
  const abortSync = async () => {
    try {
      const r = await api.gitSyncAbort(appId)
      toast(r.aborted ? 'Sync aborted' : 'Nothing to abort')
      setResolveFailed(false)
      await load()
    } catch (e) {
      onError((e as Error).message)
    }
  }
  const reviewChanges = async () => {
    if (rangeMode) {
      setRangeMode(false)
      return
    }
    setRangeMode(true)
    setDiffPath(null)
    setDiffLoading(true)
    setDiff('')
    try {
      const d = await api.gitRangeDiff(appId)
      setDiff(d.diff || (d.available ? '(no changes ahead of main)' : d.reason || 'unavailable'))
    } catch (e) {
      onError((e as Error).message)
      setRangeMode(false)
    } finally {
      setDiffLoading(false)
    }
  }
  const exitRange = () => setRangeMode(false)
  const dismissReview = () => {
    setReviewDismissed(true)
    setRangeMode(false)
  }

  return {
    st,
    inProgress,
    resolveTaskId,
    syncing,
    resolveFailed,
    rangeMode,
    reviewDismissed,
    sync,
    abortSync,
    reviewChanges,
    exitRange,
    dismissReview,
  }
}

export type GitSync = ReturnType<typeof useGitSync>

export function GitSyncBanner({ sync, onViewTask }: { sync: GitSync; onViewTask?: () => void }) {
  const { st, inProgress, resolveTaskId, resolveFailed, rangeMode, reviewDismissed } = sync
  const conflicts = st?.conflicts || []
  if (inProgress)
    return (
      <div
        data-testid="git-resolving"
        data-failed={resolveFailed ? 'true' : 'false'}
        style={{
          border: `1px solid ${resolveFailed ? c.bad : c.warn}`,
          borderRadius: 7,
          padding: '9px 11px',
          marginBottom: 10,
          fontSize: 12.5,
        }}
      >
        {resolveFailed ? (
          <>
            <div style={{ color: c.bad, fontWeight: 600 }}>Agent couldn't resolve the {inProgress} conflicts</div>
            <div style={{ marginTop: 6 }}>
              <Btn sm data-testid="git-abort" onClick={sync.abortSync}>
                Abort
              </Btn>
            </div>
          </>
        ) : (
          <div style={{ color: c.fg2, fontWeight: 600 }}>
            Agent resolving {conflicts.length} conflict{conflicts.length === 1 ? '' : 's'} ({inProgress})…
          </div>
        )}
        {conflicts.map((f) => (
          <div
            key={f}
            style={{
              ...mono,
              fontSize: 11.5,
              color: c.muted,
              marginTop: 3,
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              whiteSpace: 'nowrap',
            }}
          >
            {f}
          </div>
        ))}
        {resolveTaskId && onViewTask && (
          <div style={{ marginTop: 6 }}>
            <button type="button" data-testid="git-task-link" onClick={onViewTask} className="text-button">
              View task logs
            </button>
          </div>
        )}
      </div>
    )
  if (!resolveTaskId || (st?.ahead ?? 0) <= 0 || reviewDismissed) return null
  return (
    <div
      data-testid="git-review"
      style={{ border: `1px solid ${c.good}`, borderRadius: 7, padding: '9px 11px', marginBottom: 10, fontSize: 12.5 }}
    >
      <div style={{ color: c.fg2, fontWeight: 600 }}>Conflicts resolved by agent — review, then Publish</div>
      <div style={{ display: 'flex', gap: 12, marginTop: 6 }}>
        <button type="button" data-testid="git-review-toggle" onClick={sync.reviewChanges} className="text-button">
          {rangeMode ? 'Hide changes' : 'Review changes'}
        </button>
        <button type="button" onClick={sync.dismissReview} className="text-button" style={{ color: c.muted2 }}>
          Dismiss
        </button>
      </div>
    </div>
  )
}

export function GitSyncButtons({ sync, filesCount }: { sync: GitSync; filesCount: number }) {
  const { syncing, inProgress } = sync
  const disabled = !!syncing || !!inProgress
  const busyTitle = inProgress ? 'A sync is already in progress' : null
  return (
    <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
      <Btn
        data-testid="git-rebase"
        disabled={disabled}
        onClick={() => sync.sync('rebase')}
        title={
          busyTitle ??
          (filesCount ? 'Commits all changed files first, then rebases onto main' : 'Rebase onto the latest main')
        }
      >
        {syncing === 'rebase' ? 'Rebasing…' : 'Rebase on main'}
      </Btn>
      <Btn
        data-testid="git-publish"
        variant="primary"
        disabled={disabled}
        onClick={() => sync.sync('publish')}
        title={busyTitle ?? (filesCount ? 'Commits all changed files first, then publishes to main' : 'Publish to main')}
        style={{ fontSize: 13 }}
      >
        {syncing === 'publish' ? 'Publishing…' : 'Publish'}
      </Btn>
    </div>
  )
}
