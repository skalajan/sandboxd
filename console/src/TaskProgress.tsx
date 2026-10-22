import { useCallback, useEffect, useRef, useState } from 'react'
import { TaskSummary } from './api'

const TOOL_VERB: Record<string, string> = {
  bash: 'Running',
  edit: 'Editing',
  multiedit: 'Editing',
  write: 'Writing',
  read: 'Reading',
  glob: 'Searching',
  grep: 'Searching',
  todowrite: 'Planning',
  webfetch: 'Fetching',
  websearch: 'Searching the web',
}
const PHASE_LABEL: Record<string, string> = {
  build_check: 'Checking the build',
  health_check: 'Checking the preview',
}
const toolStep = (name: string, path?: string) => {
  const p = (path || '').replace(/^\/home\/sandbox\/workspace\/app\//, '')
  return `${TOOL_VERB[name.toLowerCase()] || name}${p ? ` ${p.length > 90 ? p.slice(0, 89) + '…' : p}` : ''}`
}
const clock = (ms: number) => {
  const s = Math.floor(ms / 1000)
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`
}

export interface TaskProgress {
  onStatus: (data: { phase?: string }) => void
  onTool: (data: { name?: string; path?: string }) => void
  steps: string[]
  phase: string
  elapsed: number
}

export function useTaskProgress(active: TaskSummary | null | undefined): TaskProgress {
  const [steps, setSteps] = useState<string[]>([])
  const [phase, setPhase] = useState('')
  const [now, setNow] = useState(() => Date.now())
  const [start, setStart] = useState(() => Date.now())
  const id = active?.id
  const running = !!active
  const created = active?.created_at
  const fromServer = useRef(false)
  useEffect(() => {
    setSteps([])
    setPhase('')
    setStart(Date.now())
    fromServer.current = false
    setNow(Date.now())
  }, [id])
  useEffect(() => {
    const t = created ? Date.parse(created) : NaN
    if (fromServer.current || Number.isNaN(t)) return
    setStart(t)
    fromServer.current = true
  }, [id, created])
  useEffect(() => {
    if (!running) return
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [running, id])
  const onStatus = useCallback((data: { phase?: string }) => {
    const label = data?.phase && PHASE_LABEL[data.phase]
    if (label) setPhase(label)
  }, [])
  const onTool = useCallback((data: { name?: string; path?: string }) => {
    if (data?.name) setSteps((s) => [...s, toolStep(data.name!, data.path)].slice(-50))
  }, [])
  return { onStatus, onTool, steps, phase, elapsed: Math.max(0, now - start) }
}

export function AgentSteps({ p, fallback }: { p: TaskProgress; fallback: string }) {
  const shown = p.steps.slice(-4)
  return (
    <div className="agent-steps">
      {shown.length > 0 && (
        <div className="agent-steps-list" data-testid="task-steps">
          {shown.map((s, i) => (
            <div key={p.steps.length - shown.length + i}>▸ {s}</div>
          ))}
        </div>
      )}
      <div className="agent-steps-working" data-testid="task-working">
        {p.phase || (shown.length ? 'Working…' : fallback)}
        <span aria-hidden="true"> · {clock(p.elapsed)}</span>
      </div>
    </div>
  )
}

export function TaskFailure({ t }: { t: TaskSummary }) {
  if (t.status === 'succeeded' || !t.agent_message || !t.error_message) return null
  return (
    <span className={`task-error status-${t.status}`} data-testid="task-error" title={t.error_message}>
      {t.error_message}
    </span>
  )
}
