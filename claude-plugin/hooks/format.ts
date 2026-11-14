export type Step = { step?: string; status?: string; started_at?: string; finished_at?: string }
export type Run = {
  id: string; repo?: string; ref?: string; status: string; display_status?: string
  failed_step?: string; created_at?: string; started_at?: string; finished_at?: string
  steps?: Step[]
}

export const ICON: Record<string, string> = {
  passed: '✓', failed: '✗', running: '▶', queued: '…', waiting: '…',
  cancelled: '■', interrupted: '!', error: '✗',
}
export const status = (r: Run): string => r.display_status || r.status
export const finished = (r: Run): boolean => ['passed', 'failed', 'cancelled', 'interrupted', 'error'].includes(r.status)

// Remove terminal strings before their individual controls: OSC, DCS/SOS/PM/APC,
// CSI (including C1 forms), then ESC sequences, controls and bidi/format marks.
export function clean(value: unknown): string {
  return String(value ?? '')
    .replace(/(?:\x1b\]|\x9d)[^\x07\x1b\x9c]*(?:\x07|\x1b\\|\x9c|$)/g, '')
    .replace(/(?:\x1b[P^_X]|[\x90\x98\x9e\x9f])[\s\S]*?(?:\x1b\\|\x9c|$)/g, '')
    .replace(/(?:\x1b\[|\x9b)[0-?]*[ -/]*[@-~]/g, '')
    .replace(/\x1b[ -/]*[@-~]/g, '')
    .replace(/[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/gu, '')
}

// Use a conservative one-cell alphabet for deterministic terminal columns.
// Wider glyphs, combining marks and emoji become '?' rather than moving columns.
export function clip(value: unknown, width: number): string {
  const text = Array.from(clean(value), ch => /[ -~✓✗▶…■!·—▸]/u.test(ch) ? ch : '?').join('')
  return text.length <= width ? text : text.slice(0, Math.max(0, width - 1)) + (width > 0 ? '…' : '')
}
const cell = (value: unknown, width: number): string => clip(value, width).padEnd(width)
const time = (value?: string): number | undefined => {
  const n = Date.parse(value || '')
  return Number.isFinite(n) && n > -62135596800000 ? n : undefined
}

export function duration(r: Run, now: number): string {
  const starts = (r.steps ?? []).map(s => time(s.started_at)).filter((n): n is number => n !== undefined)
  const ends = (r.steps ?? []).map(s => time(s.finished_at)).filter((n): n is number => n !== undefined)
  const start = time(r.started_at) ?? (starts.length ? Math.min(...starts) : undefined)
  const end = finished(r) ? time(r.finished_at) ?? (ends.length ? Math.max(...ends) : undefined) : now
  if (start === undefined || end === undefined || end < start) return '—'
  const seconds = Math.floor((end - start) / 1000)
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  return minutes < 60 ? `${minutes}m` : `${Math.floor(minutes / 60)}h${String(minutes % 60).padStart(2, '0')}`
}

export function age(r: Run, now: number): string {
  const created = time(r.created_at)
  if (created === undefined || created > now) return '—'
  const minutes = Math.floor((now - created) / 60000)
  return minutes < 60 ? `${minutes}m ago` : minutes < 1440 ? `${Math.floor(minutes / 60)}h ago` : `${Math.floor(minutes / 1440)}d ago`
}

const COLUMNS = [9, 16, 22, 14, 7, 9, 20]
export function table(runs: Run[], now: number): string {
  if (!runs.length) return 'No runs.'
  const row = (values: unknown[]): string => values.map((v, i) => cell(v, COLUMNS[i] ?? 0)).join(' ')
  return [row(['RUN', 'REPO', 'REF', 'STATUS', 'TIME', 'WHEN', 'FAILED STEP']), ...runs.map(r => row([
    clean(r.id).slice(0, 8), r.repo, r.ref, `${ICON[status(r)] ?? '·'} ${status(r)}`,
    duration(r, now), age(r, now), r.failed_step,
  ]))].join('\n')
}

export function watchLine(r: Run, now: number): string {
  const steps = r.steps ?? []
  const done = steps.filter(s => s.status === 'passed' || s.status === 'skipped').length
  const step = r.failed_step || steps.find(s => s.status === 'running')?.step || ''
  return `${clip(clean(r.id).slice(0, 8), 8)} ${cell(r.repo, 16)} ${cell(r.ref, 22)} ${ICON[status(r)] ?? '·'} ${clip(status(r), 12)} ${duration(r, now)} · ${done}/${steps.length} steps ${clip(step, 24)}`
}

export function statusLine(runs: Run[]): string {
  const running = runs.filter(r => status(r) === 'running').length
  const last = runs.find(finished)
  return `Oberth ▸ ${running} running · ${last ? `last ${ICON[status(last)] ?? '·'} ${clip(last.ref, 30)}` : 'no finished runs'}`
}

export function limit(value: unknown): number {
  const n = Number(value)
  return Number.isFinite(n) && n > 0 ? Math.min(50, Math.max(1, Math.floor(n))) : 5
}
