import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, Timer } from 'claude-code'
import type { OberthWatch } from '../types/index'
import { getRun, listRuns, ReadError } from './client'
import { clip, finished, limit, status, statusLine, table, watchLine } from './format'

const watched = atom({ plugin: 'oberth', key: 'watch' } as const, null as OberthWatch | null, { shape: 'watch-1' })
const message = (err: unknown): string => err instanceof ReadError ? err.message : 'Oberth is unavailable; try again later.'

let active = true
let generation = 0
let busy = false
let stop: Timer | undefined

async function refreshWatch($: EngineInterface): Promise<void> {
  const w = await read($, watched)
  if (!w || w.done || !active) return
  const current = generation
  try {
    const r = await getRun($, w.id)
    if (!active || current !== generation) return
    const line = watchLine(r, await $.clock.now())
    await update($, watched, prior => active && current === generation
      ? { id: r.id, line, status: status(r), done: finished(r) } : prior)
    if (active && current === generation && finished(r)) await $.ui.toast(`Oberth ${clip(line, 100)}`)
  } catch (err) {
    if (!active || current !== generation) return
    if (err instanceof ReadError && (err.kind === 'missing' || err.kind === 'setup')) {
      generation++
      await update($, watched, () => null)
      await $.ui.toast(message(err))
    }
  }
}

async function refresh($: EngineInterface): Promise<void> {
  if (busy || !active) return
  busy = true
  try {
    await refreshWatch($)
    if (!active) return
    try {
      const runs = await listRuns($, 50)
      if (active) await $.ui.status(statusLine(runs))
    } catch (err) { if (active) await $.ui.status(message(err)) }
  } finally { busy = false }
}

async function runs($: EngineInterface, count: number): Promise<string> {
  // run_list omits steps. Hydrate serially for terminal timing evidence.
  const rows = await listRuns($, count)
  const detailed = []
  for (const row of rows) {
    const timed = row.repo && row.started_at && (!finished(row) || row.finished_at)
    detailed.push(timed ? row : { ...row, ...await getRun($, row.id) })
  }
  return table(detailed, await $.clock.now())
}

async function watch($: EngineInterface, requested: string): Promise<string> {
  // /clear ends a session without another session.start in the same process.
  // Only an explicit new watch restarts polling in that new conversation.
  if (!active) {
    active = true
    stop = $.clock.every(15000, () => { void refresh($) })
  }
  const current = ++generation
  await update($, watched, () => null)
  const id = requested.trim() || (await listRuns($, 1))[0]?.id
  if (!id) return 'No runs.'
  const r = await getRun($, id)
  if (!active || current !== generation) return 'Oberth watch stopped.'
  const line = watchLine(r, await $.clock.now())
  await update($, watched, prior => active && current === generation
    ? { id: r.id, line, status: status(r), done: finished(r) } : prior)
  return `${finished(r) ? 'Finished' : 'Watching'} ${line}`
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    active = true
    await $.tool.register({ name: 'runs', description: 'Show recent Oberth runs as a read-only table.', isDeferred: false,
      inputSchema: { type: 'object', properties: { limit: { type: 'integer', minimum: 1, maximum: 50 } } } })
    await $.tool.register({ name: 'watch', description: 'Watch an Oberth run every 15 seconds; omit id for the latest run. Read-only.', isDeferred: false,
      inputSchema: { type: 'object', properties: { id: { type: 'string' } } } })
    stop?.cancel()
    stop = $.clock.every(15000, () => { void refresh($) })
    void refresh($)
    return next(e)
  })

  on('session.end', async ($, e, next) => {
    active = false
    generation++
    stop?.cancel()
    await update($, watched, () => null)
    return next(e)
  })
  on('command.run', { command: 'oberth:runs' }, async ($, e) => {
    const match = /^\s*(?:--limit[= ](\d+))?\s*$/.exec(e.args)
    if (!match) return { text: 'Usage: /oberth:runs [--limit=N]' }
    try { return { text: await runs($, limit(match[1])) } } catch (err) { return { text: message(err) } }
  })
  on('command.run', { command: 'oberth:watch' }, async ($, e) => {
    try { return { text: await watch($, e.args) } } catch (err) { return { text: message(err) } }
  })
  on('tool.call', { tool: 'mcp__oberth__runs' }, async ($, e) => {
    try { return { result: await runs($, limit(e.limit)) } } catch (err) { return { result: message(err), isError: true } }
  })
  on('tool.call', { tool: 'mcp__oberth__watch' }, async ($, e) => {
    try { return { result: await watch($, typeof e.id === 'string' ? e.id : '') } } catch (err) { return { result: message(err), isError: true } }
  })
  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const w = await read($, watched)
    if (!w || e.props.hasSurvey) return next(e)
    const { Box, Text } = $.ui.resolve(e)
    return <Box flexDirection="column">{await next(e)}<Text>{clip(w.line, e.props.bodyColumns ?? 100)}</Text></Box>
  })
  on('ui.render', { component: 'ToolResult', props: { tool: 'mcp__oberth__runs' } }, ($, e) => {
    const { Box, Text } = $.ui.resolve(e)
    return <Box flexDirection="column">{String(e.props.output ?? '').split('\n').map(line => <Text>{clip(line, 103)}</Text>)}</Box>
  })
  on('ui.render', { component: 'CommandOutput', props: { command: 'oberth:runs' } }, ($, e) => {
    const { Box, Text } = $.ui.resolve(e)
    return <Box flexDirection="column">{e.props.text.split('\n').map(line => <Text>{clip(line, 103)}</Text>)}</Box>
  })
}
