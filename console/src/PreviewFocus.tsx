import { useEffect, useState } from 'react'

const EXPAND = 'M15 3h6v6 M9 21H3v-6 M21 3l-7 7 M3 21l7-7'
const COLLAPSE = 'M21 9h-6V3 M3 15h6v6 M15 9l7-7 M9 15l-7 7'

export function PreviewFocusButton() {
  const [expanded, setExpanded] = useState(false)
  useEffect(() => {
    if (!expanded) return
    const root = document.documentElement
    root.dataset.previewExpanded = ''
    const onKey = (e: KeyboardEvent) => {
      // Dialogs cancel on their own `cancel` event, so keydown isn't defaultPrevented for them.
      if (e.key !== 'Escape' || e.defaultPrevented || document.querySelector('dialog[open]')) return
      const tag = document.activeElement?.tagName
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return
      setExpanded(false)
    }
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('keydown', onKey)
      delete root.dataset.previewExpanded
    }
  }, [expanded])
  const label = expanded ? 'Exit focus mode' : 'Focus preview'
  return (
    <button
      type="button"
      className="icon-button preview-focus"
      aria-pressed={expanded}
      aria-label={label}
      title={label}
      data-testid="preview-expand"
      onClick={() => setExpanded((v) => !v)}
    >
      <svg
        width={15}
        height={15}
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.7"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
        style={{ flexShrink: 0 }}
      >
        <path d={expanded ? COLLAPSE : EXPAND} />
      </svg>
    </button>
  )
}
