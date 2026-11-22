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

export function duration(run: Run, now: number): string {
  const start = Date.parse(run.started_at ?? run.created_at)
  const end = run.finished_at ? Date.parse(run.finished_at) : now
  if (!Number.isFinite(start) || !Number.isFinite(end) || end < start) return '—'
  const minutes = Math.round((end - start) / 60000)
  return minutes < 60 ? `${minutes}m` : `${Math.floor(minutes / 60)}h${String(minutes % 60).padStart(2, '0')}`
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

export function visible(state: Snapshot, count: number): Run[] {
  if (!state.reachable) return []
  const pins = state.pins.map(pin => pin.run)
  if (![...state.recent, ...pins].some(active)) return []
  const priority = [...pins.filter(active), ...state.recent, ...pins]
  return priority.filter((run, index) => priority.findIndex(row => row.id === run.id) === index).slice(0, count)
}

export const failedStep = (run: Run) => run.status === 'failed' ? clean(run.failure?.failed_step_name ?? '') : ''
export const outcome = (run: Run) => `${run.id}: ${run.status}${failedStep(run) ? `; step ${failedStep(run)}` : ''}`

function columns(cells: string[][], gap: number): string[] {
  const widths = cells[0]!.map((_, index) => Math.max(...cells.map(row => row[index]!.length)))
  return cells.map(row => row.map((cell, index) => cell.padEnd(widths[index]! + gap)).join('').trimEnd())
}

export function boxRows(state: Snapshot, count: number): string[] {
  const runs = visible(state, count)
  if (!runs.length) return []
  const known = [...state.recent, ...state.pins.map(pin => pin.run)]
  return columns(runs.map(run => {
    const step = failedStep(run)
    const pinned = state.pins.some(pin => pin.run.id === run.id)
    const end = duration(run, state.now) + (step ? ` [failed: ${step}]` : '') + (pinned ? ' ●' : '')
    return [ICON[run.status], shortID(run.id, known), run.repo, run.ref, progress(run), end].map(clean)
  }), 1)
}

export function table(runs: Run[], now: number): string {
  if (!runs.length) return 'No runs.'
  const header = ['RUN', 'REPO', 'REF', 'STATUS', 'STEPS', 'TIME', 'FAILED STEP', 'REASON']
  const cells = [header, ...runs.map(run => [
    run.id, run.repo, run.ref, `${ICON[run.status]} ${run.status}`, progress(run), duration(run, now), failedStep(run), run.display_reason,
  ].map(clean))]
  return columns(cells, 2).join('\n')
}

export function limit(value: unknown): number {
  const result = value === undefined || value === '' ? 5 : Number(value)
  if (!Number.isInteger(result) || result < 1 || result > 50) throw new Error('Use a limit from 1 to 50.')
  return result
}
