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
  const priority = [...pins, ...state.recent]
  return priority.filter((run, index) => priority.findIndex(row => row.id === run.id) === index).slice(0, count)
}

export const failedStep = (run: Run) => run.status === 'failed' ? clean(run.failure?.failed_step_name ?? '') : ''
export const outcome = (run: Run) => `${run.id}: ${run.status}${failedStep(run) ? `; step ${failedStep(run)}` : ''}`

const HEADER = ['RUN', 'REPO', 'REF', 'STATUS', 'STEPS', 'TIME', 'AGE']
// The last column, repository and ref (wider first) give way down to this many cells.
const NARROWEST = 4
const shortRepo = (repo: string) => repo.slice(repo.lastIndexOf('/') + 1) || repo
const shortRef = (ref: string) => ref.replace(/^refs\/(?:heads|tags)\//, '') || ref
// Terminal cells of a code point: two for wide East Asian forms and emoji, otherwise one.
const WIDE = /[\p{Emoji_Presentation}ᄀ-ᅟ⺀-〾ぁ-㏿㐀-䶿一-鿿ꥠ-꥿가-힣豈-﫿︰-﹏＀-｠￠-￦\u{20000}-\u{3FFFD}]/u
const cellsOf = (point: string) => WIDE.test(point) ? 2 : 1
const cells = (text: string) => Array.from(text).reduce((sum, point) => sum + cellsOf(point), 0)
const pad = (text: string, width: number) => text + ' '.repeat(Math.max(0, width - cells(text)))

// At most width cells, cut between code points and marked with an ellipsis.
function cut(text: string, width: number): string {
  if (cells(text) <= width) return text
  let kept = ''
  for (const point of text) {
    if (cells(kept) + cellsOf(point) > width - 1) break
    kept += point
  }
  return kept + '…'
}

// The last column: its header, each run's text, and whether it gives way before the names.
type Last = { label: string; text: (run: Run) => string; first: boolean }

// Aligned single lines of at most width cells. The last column and the names give way in
// the order Last says (the last column goes when it cannot keep NARROWEST), then lines are cut.
function fit(table: string[][], width: number, first: boolean): string[] {
  const widths = table[0]!.map((_, index) => Math.max(...table.map(row => cells(row[index]!))))
  const last = widths.length - 1
  // Cells the shown columns and the spaces between them take beyond the width.
  const over = () => widths.filter(Boolean).reduce((sum, size) => sum + size + 1, -1) - width
  const shrinkLast = () => {
    while (over() > 0 && widths[last]) widths[last] = widths[last]! > NARROWEST ? widths[last]! - 1 : 0
  }
  const shrinkNames = () => {
    while (over() > 0) {
      const index = widths[1]! >= widths[2]! ? 1 : 2
      if (widths[index]! <= NARROWEST) return
      widths[index] = widths[index]! - 1
    }
  }
  for (const shrink of first ? [shrinkLast, shrinkNames] : [shrinkNames, shrinkLast]) shrink()
  return table.map(row => cut(row.flatMap((cell, index) =>
    widths[index] ? [pad(cut(cell, widths[index]!), widths[index]!)] : []).join(' ').trimEnd(), width))
}

// The header and one line per run, shared by the box and /oberth-runs.
function rows(runs: Run[], now: number, width: number, last: Last): string[] {
  return fit([[...HEADER, last.label], ...runs.map(run => [
    shortID(run.id, runs), shortRepo(run.repo), shortRef(run.ref), `${ICON[run.status]} ${run.status}`,
    progress(run), duration(run, now), age(run, now), last.text(run),
  ].map(clean))], width, last.first)
}

// The box ends a row with ● for a watched run and the failed step's name; both give way
// before the names.
export function boxRows(state: Snapshot, count: number, width: number): string[] {
  const runs = visible(state, count)
  const watched = (run: Run) => state.pins.some(pin => pin.run.id === run.id) ? '●' : ''
  const text = (run: Run) => [watched(run), failedStep(run)].filter(Boolean).join(' ')
  return runs.length ? rows(runs, state.now, width, { label: '', text, first: true }) : []
}

// /oberth-runs ends a failed row with its failed step and reason and a cancelled row with
// its reason, kept before the names; other reasons only repeat the status word.
const explained = (run: Run) => run.status === 'failed' || run.status === 'cancelled'
const reason = (run: Run) => explained(run) ? [failedStep(run), run.display_reason].filter(Boolean).join(': ') : ''

export function table(runs: Run[], now: number, width: number): string {
  return runs.length ? rows(runs, now, width, { label: 'REASON', text: reason, first: false }).join('\n') : 'No runs.'
}

export function limit(value: unknown): number {
  const result = value === undefined || value === '' ? 5 : Number(value)
  if (!Number.isInteger(result) || result < 1 || result > 50) throw new Error('Use a limit from 1 to 50.')
  return result
}
