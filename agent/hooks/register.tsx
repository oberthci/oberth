import { atom, read, update } from 'claude-code'
import type { EngineInterface, On, PluginOptions, Register, Timer } from 'claude-code'
import type { Snapshot } from '../types'
import { executable, get, list, profile, SETUP, PROFILE_SETUP } from './client'
import { active, boxRows, limit, outcome, shown, SHOWN_REASON, SHOWN_REMEDY, table, visible } from './model'

const empty: Snapshot = { recent: [], pins: [], reachable: false, now: 0 }
const snapshot = atom({ plugin: 'oberth', key: 'snapshot' } as const, empty, { shape: 'runs-1' })
const errorText = (error: unknown) => error instanceof Error ? error.message : 'Unable to read Oberth runs.'
// count is how many runs the box shows; with none, only active watches are read.
type Loop = { count: number; timer?: Timer; pending?: Promise<void>; stopped?: boolean; refreshedAt?: number }

async function tick($: EngineInterface, options: PluginOptions, loop: Loop) {
  const state = await read($, snapshot)
  if (!loop.count && !state.pins.some(pin => active(pin.run))) return
  const interval = [...state.recent, ...state.pins.map(pin => pin.run)].some(active) ? 15000 : 60000
  if (await $.clock.now() - (loop.refreshedAt ?? -Infinity) >= interval) await poll($, options, loop)
}

// Background refreshes are never awaited by the host: a refused state call (for example
// after the module unloads) ends the attempt instead of becoming an unhandled rejection.
function poll($: EngineInterface, options: PluginOptions, loop: Loop) {
  if (!loop.pending) loop.pending = refresh($, options, loop).catch(() => {}).finally(() => { loop.pending = undefined })
  return loop.pending
}

async function refresh($: EngineInterface, options: PluginOptions, loop: Loop) {
  try {
    loop.refreshedAt = await $.clock.now()
    const recent = loop.count ? await list($, options, loop.count) : []
    const before = await read($, snapshot)
    const missing = before.pins.filter(pin => active(pin.run) && !recent.some(run => run.id === pin.run.id))
    const details = await Promise.all(missing.map(pin => get($, options, pin.run.id)))
    const now = await $.clock.now()
    if (loop.stopped) return
    const finished: string[] = []
    await update($, snapshot, state => {
      // A compare-and-set miss retries this callback; discard that attempt's notices.
      finished.length = 0
      if (loop.stopped) return state
      return {
        recent, reachable: true, now,
        pins: state.pins.map(pin => {
          if (pin.doneAt !== null) return pin
          const run = [...recent, ...details].find(run => run.id === pin.run.id) ?? pin.run
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

async function watch($: EngineInterface, options: PluginOptions, id: string): Promise<string> {
  const run = await get($, options, id)
  if (!active(run)) return `${outcome(run)}; already finished.`
  await update($, snapshot, state => {
    const pins = state.pins.filter(pin => active(pin.run) && pin.run.id !== run.id)
    if (pins.length === 3) throw new Error('Three runs are already pinned. Wait for one to finish.')
    return { ...state, reachable: true, pins: [...pins, { run, doneAt: null }] }
  })
  return `Watching ${run.id} until it finishes.`
}

function commands(on: On, options: PluginOptions) {
  on('command.run', { command: 'oberth-runs' }, async ($, e) => {
    try {
      const arg = e.args.trim()
      const match = /^(?:--limit[= ](\d+))?$/.exec(arg)
      if (!match) throw new Error('Use /oberth-runs [--limit=N].')
      return { text: table(await list($, options, limit(match[1])), await $.clock.now()) }
    } catch (error) { return { text: errorText(error) } }
  })
  on('command.run', { command: 'oberth-watch' }, async ($, e) => {
    try { return { text: await watch($, options, e.args.trim()) } }
    catch (error) { return { text: errorText(error) } }
  })
  on('tool.call', { tool: 'mcp__oberth__runs' }, async ($, e) => {
    try {
      const input = e as typeof e & { limit?: number }
      return { result: table(await list($, options, limit(input.limit)), await $.clock.now()) }
    } catch (error) { return { result: errorText(error), isError: true } }
  })
  on('tool.call', { tool: 'mcp__oberth__watch' }, async ($, e) => {
    try {
      const input = e as typeof e & { id?: string }
      return { result: await watch($, options, typeof input.id === 'string' ? input.id.trim() : '') }
    } catch (error) { return { result: errorText(error), isError: true } }
  })
}

async function registerCommands($: EngineInterface) {
  await $.command.register({ name: 'oberth-runs', description: 'Show recent Oberth runs (/oberth-runs [--limit=N]).' })
  await $.command.register({ name: 'oberth-watch', description: 'Pin a run until completion (/oberth-watch <id or prefix>).' })
}

// The host serves these as MCP server "oberth" and refuses them while the session has the
// joined bridge under that name; the bridge's tools then serve the model, and the box goes on.
async function registerTools($: EngineInterface) {
  const tools = [{
    name: 'runs', description: 'Show recent Oberth runs with whole fields and authoritative plan counts.',
    inputSchema: { type: 'object', properties: { limit: { type: 'integer', minimum: 1, maximum: 50 } } },
    isDeferred: false,
  }, {
    name: 'watch', description: 'Pin an Oberth run above the prompt until it finishes, with one completion toast.',
    inputSchema: { type: 'object', properties: { id: { type: 'string' } }, required: ['id'] },
    isDeferred: false,
  }]
  for (const tool of tools) await $.tool.register(tool).catch(() => undefined)
}

// The setup notice the box needs before it can read runs, or none.
async function missingSetup($: EngineInterface, options: PluginOptions): Promise<{ key: string; text: string } | undefined> {
  try { executable(options) } catch { return { key: 'executable-notice', text: SETUP } }
  try { await profile($, options) } catch { return { key: 'profile-notice', text: PROFILE_SETUP } }
  return undefined
}

async function setupNotice($: EngineInterface, key: string, message: string, mark: unknown = true) {
  if (await $.store.get(key) === mark) return
  await $.store.set(key, mark)
  await $.ui.toast(message)
}

export const register: Register = (on, options) => {
  const count = shown(options.runs)
  const loop: Loop = { count: count ?? 0 }
  on('session.start', async ($, e, next) => {
    loop.timer?.cancel()
    loop.stopped = false
    await registerCommands($)
    if (count === undefined) await setupNotice($, 'runs-notice', `${SHOWN_REASON} ${SHOWN_REMEDY}`, String(options.runs))
    const missing = await missingSetup($, options)
    if (missing) {
      await update($, snapshot, () => empty)
      await setupNotice($, missing.key, missing.text)
    } else {
      if (loop.count) void poll($, options, loop)
      loop.timer = $.clock.every(15000, () => { void tick($, options, loop).catch(() => {}) })
    }
    await registerTools($)
    return next(e)
  })
  on('session.end', async ($, e, next) => {
    if (e.reason === 'clear' || e.reason === 'resume') return next(e)
    loop.stopped = true
    loop.timer?.cancel()
    await update($, snapshot, () => empty)
    return next(e)
  })
  commands(on, options)
  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    if (e.props.hasSurvey) return next(e)
    const state = await read($, snapshot)
    const rows = visible(state, loop.count)
    if (!rows.length) return next(e)
    const lines = boxRows(state, loop.count)
    const { Box, Text } = $.ui.resolve(e)
    const color = (status: string) => status === 'passed' ? 'green' : status === 'failed' ? 'red' : status === 'cancelled' ? 'gray' : 'yellow'
    return <Box flexDirection="column" alignSelf="flex-start" borderStyle="round" borderColor="gray" paddingX={1}>
      {rows.map((run, index) => <Text wrap="wrap" color={color(run.status)}>{lines[index]}</Text>)}
    </Box>
  })
}
