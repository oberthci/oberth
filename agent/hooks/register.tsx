import { atom, read, update } from 'claude-code'
import type { EngineInterface, On, PluginOptions, Register, Timer } from 'claude-code'
import type { Snapshot } from '../types'
import { checkedExecutable, get, list, profiles, SETUP, waitForChange } from './client'
import { active, boxRows, limit, outcome, profileProblems, REFRESH_MS, shown, SHOWN_REASON, SHOWN_REMEDY, table, visible } from './model'

const empty: Snapshot = { recent: [], pins: [], reachable: false, now: 0 }
const snapshot = atom({ plugin: 'oberth', key: 'snapshot' } as const, empty, { shape: 'runs-1' })
// The box's round border and one cell of padding on each side.
const FRAME = 4
const errorText = (error: unknown) => error instanceof Error ? error.message : 'Unable to read Oberth runs.'
// count is how many runs the box shows; with none, only active watches are read.
type Loop = { count: number; timer?: Timer; pending?: Promise<void>; stopped?: boolean }

async function tick($: EngineInterface, options: PluginOptions, loop: Loop) {
  if (loop.stopped || loop.pending) return
  const state = await read($, snapshot)
  if (!loop.count && !state.pins.some(pin => active(pin.run))) return
  await poll($, options, loop)
}

// Background refreshes are never awaited by the host: a refused state call (for example
// after the module unloads) ends the attempt instead of becoming an unhandled rejection.
function poll($: EngineInterface, options: PluginOptions, loop: Loop) {
  if (!loop.pending) loop.pending = follow($, options, loop).catch(() => {}).finally(() => { loop.pending = undefined })
  return loop.pending
}

async function follow($: EngineInterface, options: PluginOptions, loop: Loop) {
  while (!loop.stopped) {
    await refresh($, options, loop)
    const state = await read($, snapshot)
    if (loop.stopped || !state.reachable) return
    const runs = [...state.recent, ...state.pins.map(pin => pin.run)]
      .filter(run => active(run) && run.profile && !state.unavailable?.includes(run.profile))
      .filter((run, index, all) => all.findIndex(row => row.id === run.id) === index)
    if (!runs.length) return
    const changes = await Promise.allSettled(runs.map(run => waitForChange($, options, run.id, run.profile!)))
    // A failed change feed falls back to the idle timer, never a busy loop.
    if (loop.stopped || changes.every(change => change.status === 'rejected')) return
  }
}

async function refresh($: EngineInterface, options: PluginOptions, loop: Loop) {
  try {
    const page = loop.count ? await list($, options, loop.count) : { runs: [], clocks: [], unavailable: [] }
    const recent = page.runs
    const before = await read($, snapshot)
    const clocks = [...page.clocks, ...(before.clocks ?? [])].filter((clock, index, all) => all.findIndex(row => row.profile === clock.profile) === index)
    const missing = before.pins.filter(pin => active(pin.run) && !recent.some(run => run.id === pin.run.id))
    const details = await Promise.allSettled(missing.map(pin => get($, options, pin.run.id, pin.run.profile)))
    const unavailable = new Set(page.unavailable)
    details.forEach((detail, index) => {
      const source = missing[index]!.run.profile
      if (source && detail.status === 'rejected') unavailable.add(source)
    })
    const observed = [...recent, ...details.flatMap(detail => detail.status === 'fulfilled' ? [detail.value] : [])]
    const now = await $.clock.now()
    if (loop.stopped) return
    const finished: string[] = []
    await update($, snapshot, state => {
      // A compare-and-set miss retries this callback; discard that attempt's notices.
      finished.length = 0
      if (loop.stopped) return state
      // Rows age on the server's clock, which stamped them; watches expire on this one.
      return {
        recent, reachable: true, now, clocks, unavailable: [...unavailable],
        pins: state.pins.map(pin => {
          const observedRun = observed.find(run => run.id === pin.run.id) ?? pin.run
          const clockOffset = clocks.find(clock => clock.profile === observedRun.profile)?.offset
          const run = { ...observedRun, clockOffset }
          if (pin.doneAt !== null) return { ...pin, run }
          const doneAt = active(run) ? null : now
          if (doneAt !== null) finished.push(outcome(run))
          return { run, doneAt }
        }).filter(pin => pin.doneAt === null || now - pin.doneAt < 180000),
      }
    })
    for (const text of finished) await $.ui.toast(`Oberth ${text}`)
  } catch {
    if (!loop.stopped) await update($, snapshot, state => ({ ...state, recent: [], reachable: false }))
  }
}

async function watch($: EngineInterface, options: PluginOptions, id: string, loop: Loop): Promise<string> {
  const run = await get($, options, id)
  if (!active(run)) return `${outcome(run)}; already finished.`
  await update($, snapshot, state => {
    if (loop.stopped) return state
    const pins = state.pins.filter(pin => active(pin.run) && pin.run.id !== run.id)
    if (pins.length === 3) throw new Error('Three runs are already pinned. Wait for one to finish.')
    const clockOffset = state.clocks?.find(clock => clock.profile === run.profile)?.offset
    return { ...state, reachable: true, pins: [...pins, { run: { ...run, clockOffset }, doneAt: null }] }
  })
  if (!loop.stopped) void poll($, options, loop)
  return `Watching ${run.id} until it finishes.`
}

function commands(on: On, options: PluginOptions, current: () => Loop) {
  on('command.run', { command: 'oberth-runs' }, async ($, e) => {
    try {
      const arg = e.args.trim()
      const match = /^(?:--limit[= ](\d+))?$/.exec(arg)
      if (!match) throw new Error('Use /oberth-runs [--limit=N].')
      const page = await list($, options, limit(match[1]))
      return { text: [table(page.runs, await $.clock.now(), e.presentation.columns), ...profileProblems(page.unavailable)].join('\n') }
    } catch (error) { return { text: errorText(error) } }
  })
  on('command.run', { command: 'oberth-watch' }, async ($, e) => {
    try { return { text: await watch($, options, e.args.trim(), current()) } }
    catch (error) { return { text: errorText(error) } }
  })
}

// The person's commands only: model tools here would be MCP server "oberth", the joined
// bridge's name, and the bridge already serves the model.
async function registerCommands($: EngineInterface) {
  await $.command.register({ name: 'oberth-runs', description: 'Show recent Oberth runs (/oberth-runs [--limit=N]).' })
  await $.command.register({ name: 'oberth-watch', description: 'Pin a run until completion (/oberth-watch <id or prefix>).' })
}

// The setup notice the box needs before it can read runs, or none.
async function missingSetup($: EngineInterface, options: PluginOptions): Promise<{ key: string; text: string; mark?: string } | undefined> {
  try { await checkedExecutable($, options) } catch { return { key: 'executable-notice', text: SETUP } }
  try { await profiles($, options) } catch (error) { return { key: 'profile-notice', text: errorText(error), mark: String(options.profile) } }
  return undefined
}

async function setupNotice($: EngineInterface, key: string, message: string, mark: unknown = true) {
  if (await $.store.get(key) === mark) return
  await $.store.set(key, mark)
  await $.ui.toast(message)
}

export const register: Register = (on, options) => {
  const count = shown(options.runs)
  let loop: Loop = { count: count ?? 0 }
  on('session.start', async ($, e, next) => {
    loop.stopped = true
    loop.timer?.cancel()
    const current: Loop = { count: count ?? 0 }
    loop = current
    await registerCommands($)
    if (count === undefined) await setupNotice($, 'runs-notice', `${SHOWN_REASON} ${SHOWN_REMEDY}`, String(options.runs))
    const missing = await missingSetup($, options)
    if (current.stopped) return next(e)
    if (missing) {
      await update($, snapshot, () => empty)
      await setupNotice($, missing.key, missing.text, missing.mark)
    } else {
      if (current.count) void poll($, options, current)
      current.timer = $.clock.every(REFRESH_MS, () => { void tick($, options, current).catch(() => {}) })
    }
    return next(e)
  })
  on('session.end', async ($, e, next) => {
    if (e.reason === 'clear' || e.reason === 'resume') return next(e)
    loop.stopped = true
    loop.timer?.cancel()
    await update($, snapshot, () => empty)
    return next(e)
  })
  commands(on, options, () => loop)
  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    if (e.props.hasSurvey) return next(e)
    const state = await read($, snapshot)
    const rows = visible(state, loop.count)
    const problems = loop.count ? profileProblems(state.unavailable ?? []) : []
    if (!rows.length && !problems.length) return next(e)
    const columns = e.props.bodyColumns
    const problemWidth = Number.isFinite(columns) && columns > FRAME ? Math.floor(columns) : 84
    const [header, ...lines] = boxRows(state, loop.count, e.props.bodyColumns - FRAME)
    const { Box, Text } = $.ui.resolve(e)
    const color = (status: string) => status === 'passed' ? 'green' : status === 'failed' ? 'red' : status === 'cancelled' ? 'gray' : 'yellow'
    return <Box flexDirection="column" alignSelf="flex-start" borderStyle="round" borderColor="gray" paddingX={1} width={problems.length ? problemWidth : undefined}>
      {rows.length > 0 && <Text wrap="truncate-end" dimColor>{header}</Text>}
      {rows.map((run, index) => <Text wrap="truncate-end" color={color(run.status)}>{lines[index]}</Text>)}
      {problems.map(problem => <Text wrap="wrap" color="yellow">{problem}</Text>)}
    </Box>
  })
}
