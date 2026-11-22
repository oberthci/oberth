import type { Run, Snapshot } from '../types'

// One refresh interval, active or idle: a run shorter than it can start and finish unseen.
export const REFRESH_MS = 15000
export const ICON ={ queued: '…', running: '▶', passed: '✓', failed: '✗', cancelled: '■' }
export const active = (run: Run) => run.status === 'queued' || run.status === 'running'
const clean = (text: string) => text.replace(/[\p{Cc}\p{Cf}]/gu, '')
export const progress = (run: Run) =>
  `${run.passed + run.failed + run.skipped}/${run.steps_planned_exact ? '' : '≥'}${run.step_count}`

export function decodeRun(value: unknown): Run {
  const run = value as Run
  if (!run || !Object.hasOwn(ICON, run.status)) throw new Error('Invalid run JSON.')
  for (const field of ['id', 'repo', 'ref', 'display_reason', 'created_at'] as const) {
    if (typeof run[field] !== 'string') throw new Error('Invalid run JSON.')
  }
  for (const field of ['started_at', 'finished_at'] as const) {
    if (run[field] !== null && typeof run[field] !== 'string') throw new Error('Invalid run JSON.')
  }
  for (const field of ['step_count', 'passed', 'failed', 'skipped'] as const) {
    if (!Number.isSafeInteger(run[field]) || run[field] < 0) throw new Error('Invalid step counts.')
  }
  if (run.failure != null && (typeof run.failure.failed_step !== 'string' || typeof run.failure.failed_step_name !== 'string')) {
    throw new Error('Invalid failure JSON.')
  }
  if (typeof run.steps_planned_exact !== 'boolean') throw new Error('Invalid step counts.')
  return run
}

const UNITS: [number, string][] = [[86400, 'd'], [3600, 'h'], [60, 'm'], [1, 's']]

// 42s, 3m10s, 2h05m, 1d04h with two units; 42s, 3m, 2h, 1d with one.
export function span(ms: number, units: 1 | 2): string {
  const seconds = Math.max(0, Math.floor(ms / 1000))
  const index = UNITS.findIndex(([size]) => seconds >= size)
  if (index < 0) return '0s'
  const [size, unit] = UNITS[index]!
  const head = `${Math.floor(seconds / size)}${unit}`
  const below = UNITS[index + 1]
  if (units === 1 || !below) return head
  return head + String(Math.floor(seconds % size / below[0])).padStart(2, '0') + below[1]
}

export function duration(run: Run, now: number): string {
  const start = Date.parse(run.started_at ?? run.created_at)
  const end = run.finished_at ? Date.parse(run.finished_at) : now
  if (!Number.isFinite(start) || !Number.isFinite(end) || end < start) return '—'
  return span(end - start, 2)
}

export function age(run: Run, now: number): string {
  const created = Date.parse(run.created_at)
  return Number.isFinite(created) ? span(now - created, 1) : '—'
}

export function shortID(id: string, runs: Run[]): string {
  if (!/^[a-f0-9]{32}$/.test(id)) return id
  let size = 8
  while (size < id.length && runs.some(run => run.id !== id && run.id.startsWith(id.slice(0, size)))) size++
  return id.slice(0, size)
}

export const SHOWN_REASON = 'Runs shown accepts 0 to 10.'
export const SHOWN_REMEDY = 'Set runs to a whole number from 0 to 10; 3 is the default and 0 shows no run box.'

// The `runs` option: unset or blank is three; anything but a whole number from 0 to 10 is undefined.
export function shown(value: unknown): number | undefined {
  if (value === undefined || value === null || typeof value === 'string' && !value.trim()) return 3
  const count = typeof value === 'string' ? Number(value) : value
  return typeof count === 'number' && Number.isInteger(count) && count >= 0 && count <= 10 ? count : undefined
}

// A finished run keeps the box up this long, so its row is seen turning passed or failed.
export const WINDOW_MS = 600000
const lately = (run: Run, now: number) => active(run) || now - Date.parse(run.finished_at ?? '') < WINDOW_MS

export function visible(state: Snapshot, count: number): Run[] {
  if (!state.reachable) return []
  const pins = state.pins.map(pin => pin.run)
  if (![...state.recent, ...pins].some(run => lately(run, state.now))) return []
  const priority = [...pins.filter(active), ...state.recent, ...pins]
  return priority.filter((run, index) => priority.findIndex(row => row.id === run.id) === index).slice(0, count)
}

export const failedStep = (run: Run) => run.status === 'failed' ? clean(run.failure?.failed_step_name ?? '') : ''
export const outcome = (run: Run) => `${run.id}: ${run.status}${failedStep(run) ? `; step ${failedStep(run)}` : ''}`

const HEADER = ['RUN', 'REPO', 'REF', 'STATUS', 'STEPS', 'TIME', 'AGE']
// Repository and ref give way, wider first, down to this many cells each.
const NARROWEST = 4
const shortRepo = (repo: string) => repo.slice(repo.lastIndexOf('/') + 1) || repo
const shortRef = (ref: string) => ref.replace(/^refs\/(?:heads|tags)\//, '') || ref
const cut = (text: string, width: number) => text.length <= width ? text : text.slice(0, Math.max(0, width - 1)) + '…'

// Aligned single lines of at most width cells: repository and ref shrink first, then the line is cut.
function fit(cells: string[][], width: number): string[] {
  const widths = cells[0]!.map((_, index) => Math.max(...cells.map(row => row[index]!.length)))
  let over = widths.reduce((sum, size) => sum + size, widths.length - 1) - width
  while (over > 0) {
    const index = widths[1]! >= widths[2]! ? 1 : 2
    if (widths[index]! <= NARROWEST) break
    widths[index] = widths[index]! - 1
    over--
  }
  return cells.map(row => cut(row.map((cell, index) => cut(cell, widths[index]!).padEnd(widths[index]!)).join(' ').trimEnd(), width))
}

// The header and one line per run, shared by the box and /oberth-runs.
export function rows(runs: Run[], now: number, width: number): string[] {
  return fit([HEADER, ...runs.map(run => [
    shortID(run.id, runs), shortRepo(run.repo), shortRef(run.ref), `${ICON[run.status]} ${run.status}`,
    progress(run), duration(run, now), age(run, now),
  ].map(clean))], width)
}

export function boxRows(state: Snapshot, count: number, width: number): string[] {
  const runs = visible(state, count)
  return runs.length ? rows(runs, state.now, width) : []
}

export function table(runs: Run[], now: number, width: number): string {
  return runs.length ? rows(runs, now, width).join('\n') : 'No runs.'
}

export function limit(value: unknown): number {
  const result = value === undefined || value === '' ? 5 : Number(value)
  if (!Number.isInteger(result) || result < 1 || result > 50) throw new Error('Use a limit from 1 to 50.')
  return result
}
