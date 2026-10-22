import { c, font } from './kit'
import logo from './proceram-logo.svg'

export const REPO_URL = 'https://github.com/skalajan/sandboxd'

export function Brand({ light = false }: { light?: boolean }) {
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
      <img src={logo} alt="ProCeram" style={{ height: 17, display: 'block', filter: light ? 'brightness(0) invert(1)' : undefined }} />
      <span style={{ fontFamily: font.display, fontWeight: 500, fontSize: 15, letterSpacing: '.2px', color: light ? '#b1c7a5' : c.muted }}>DevBox</span>
    </div>
  )
}
