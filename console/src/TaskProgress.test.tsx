import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, act, cleanup } from '@testing-library/react'
import { AgentSteps, TaskFailure, useTaskProgress } from './TaskProgress'
import { TaskSummary } from './api'

const task = (over: Partial<TaskSummary> = {}): TaskSummary => ({ id: 'T1', status: 'running', can_revert: false, ...over })

let latest: ReturnType<typeof useTaskProgress>
function Harness({ active, fallback = 'Building' }: { active: TaskSummary | null; fallback?: string }) {
  latest = useTaskProgress(active)
  return <AgentSteps p={latest} fallback={fallback} />
}

afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

describe('useTaskProgress', () => {
  it('labels tool steps, strips paths and shows the last 4', () => {
    render(<Harness active={task()} />)
    act(() => {
      latest.onTool({ name: 'Edit', path: '/home/sandbox/workspace/app/src/routes/index.tsx' })
      latest.onTool({ name: 'Bash', path: 'bun run check:fast' })
    })
    expect(screen.getByTestId('task-steps').textContent).toBe('▸ Editing src/routes/index.tsx▸ Running bun run check:fast')
    act(() => {
      for (const n of ['a', 'b', 'c']) latest.onTool({ name: 'Read', path: n })
    })
    const text = screen.getByTestId('task-steps').textContent
    expect(text).toBe('▸ Running bun run check:fast▸ Reading a▸ Reading b▸ Reading c')
  })

  it('caps steps at 50', () => {
    render(<Harness active={task()} />)
    act(() => {
      for (let i = 0; i < 60; i++) latest.onTool({ name: 'Read', path: String(i) })
    })
    expect(latest.steps).toHaveLength(50)
    expect(latest.steps[49]).toBe('Reading 59')
  })

  it('maps a status phase to its label and falls back otherwise', () => {
    render(<Harness active={task()} fallback="Building" />)
    expect(screen.getByTestId('task-working').textContent).toMatch(/^Building · /)
    act(() => latest.onStatus({ phase: 'build_check' }))
    expect(screen.getByTestId('task-working').textContent).toMatch(/^Checking the build · /)
    act(() => latest.onStatus({ phase: 'unknown' }))
    expect(screen.getByTestId('task-working').textContent).toMatch(/^Checking the build · /)
  })

  it('shows elapsed m:ss from created_at and ticks', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-01-01T00:05:00Z'))
    render(<Harness active={task({ created_at: '2026-01-01T00:03:55Z' })} />)
    expect(screen.getByTestId('task-working').textContent).toBe('Building · 1:05')
    act(() => {
      vi.advanceTimersByTime(3000)
    })
    expect(screen.getByTestId('task-working').textContent).toBe('Building · 1:08')
  })

  it('counts from first sight without created_at', () => {
    vi.useFakeTimers()
    render(<Harness active={task()} />)
    expect(screen.getByTestId('task-working').textContent).toBe('Building · 0:00')
    act(() => {
      vi.advanceTimersByTime(65000)
    })
    expect(screen.getByTestId('task-working').textContent).toBe('Building · 1:05')
  })

  it('adopts a late created_at without clearing steps', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-01-01T00:05:00Z'))
    const { rerender } = render(<Harness active={task()} />)
    act(() => latest.onTool({ name: 'Read', path: 'x' }))
    rerender(<Harness active={task({ created_at: '2026-01-01T00:04:50Z' })} />)
    expect(screen.getByTestId('task-steps').textContent).toContain('Reading x')
    expect(screen.getByTestId('task-working').textContent).toBe('Working… · 0:10')
  })

  it('resets steps, phase and start when the task id changes', () => {
    vi.useFakeTimers()
    const { rerender } = render(<Harness active={task()} />)
    act(() => {
      latest.onTool({ name: 'Read', path: 'x' })
      latest.onStatus({ phase: 'health_check' })
      vi.advanceTimersByTime(5000)
    })
    expect(screen.getByTestId('task-working').textContent).toBe('Checking the preview · 0:05')
    rerender(<Harness active={task({ id: 'T2' })} />)
    expect(screen.queryByTestId('task-steps')).toBeNull()
    expect(screen.getByTestId('task-working').textContent).toBe('Building · 0:00')
  })

  it('keeps onStatus/onTool referentially stable', () => {
    const { rerender } = render(<Harness active={task()} />)
    const { onStatus, onTool } = latest
    act(() => latest.onTool({ name: 'Read', path: 'x' }))
    rerender(<Harness active={task({ id: 'T2' })} />)
    expect(latest.onStatus).toBe(onStatus)
    expect(latest.onTool).toBe(onTool)
  })
})

describe('TaskFailure', () => {
  const failed = task({ status: 'failed', agent_message: 'Waiting for lint…', error_message: 'agent timed out after 10m' })

  it('renders the reason for a non-succeeded turn with both messages', () => {
    render(<TaskFailure t={failed} />)
    expect(screen.getByTestId('task-error').textContent).toBe('agent timed out after 10m')
  })

  it.each([
    ['succeeded', { ...failed, status: 'succeeded' }],
    ['no agent message', { ...failed, agent_message: undefined }],
    ['no error message', { ...failed, error_message: undefined }],
  ])('renders nothing for %s', (_n, t) => {
    render(<TaskFailure t={t} />)
    expect(screen.queryByTestId('task-error')).toBeNull()
  })
})
