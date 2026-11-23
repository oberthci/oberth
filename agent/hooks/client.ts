import type { EngineInterface, PluginOptions } from 'claude-code'
import { decodeRun } from './model'
import type { ObservedRun } from '../types'

export const SETUP = 'Set executable to the absolute verified path printed by oberth join.'
export const PROFILE_SETUP = 'Set profile to the uplink profile from oberth join, set OBERTH_PROFILE or start in the joined checkout.'
// Where fetch-oberth installs the verified CLI, beneath the person's home directory.
const INSTALLED = '/.local/bin/oberth'
// The checkout binding join writes; the bridge reads the same key when started without --profile.
const BINDING = ['git', 'config', '--local', '--get', 'oberth.uplinkProfile']

const chosen = (value: unknown) => typeof value === 'string' && value.trim() ? value : ''

async function bound($: EngineInterface): Promise<string> {
  const out = await $.process.run(BINDING, { cwd: await $.session.root(), timeoutMs: 3500 }).catch(() => undefined)
  return out?.exitCode === 0 && !out.isStdoutTruncated ? out.stdout.trim() : ''
}

// The profile option, then OBERTH_PROFILE, then the checkout's joined uplink.
export async function profile($: EngineInterface, options: PluginOptions): Promise<string> {
  const name = chosen(options.profile) || chosen(await $.env.get('OBERTH_PROFILE')) || await bound($)
  if (!name) throw new Error(PROFILE_SETUP)
  return name
}

// An explicit list replaces the single-profile selection; credentials stay with the CLI.
export async function profiles($: EngineInterface, options: PluginOptions): Promise<string[]> {
  const configured = options.profiles as string | undefined
  if (!configured?.trim()) return [await profile($, options)]
  const names = [...new Set(configured.split(',').map(name => name.trim()))]
  if (names.includes('') || names.length > 32) throw new Error('Set profiles to 1 to 32 comma-separated joined profile names.')
  return names
}

// The executable option, else the fetcher's install under HOME; always an absolute path.
async function executable($: EngineInterface, options: PluginOptions): Promise<string> {
  const home = chosen(await $.env.get('HOME'))
  const path = chosen(options.executable) || (home && home.replace(/\/+$/, '') + INSTALLED)
  if (!/^(\/|[A-Za-z]:[\\/]|\\\\)/.test(path) || path.includes('\0')) throw new Error(SETUP)
  return path
}

export async function checkedExecutable($: EngineInterface, options: PluginOptions): Promise<string> {
  const path = await executable($, options)
  const cwd = await $.session.root()
  const [file, directory, checkout] = await Promise.all([
    $.fs.stat(path, { resolve: true }).catch(() => { throw new Error(SETUP) }), $.fs.stat(cwd, { resolve: true }),
    $.fs.stat(cwd + '/.git').catch(() => ({ kind: 'other' as const })),
  ])
  if (file.kind !== 'file' || !file.realPath || !directory.realPath) throw new Error(SETUP)
  const root = directory.realPath.replace(/[\\/]+$/, '')
  if (checkout.kind !== 'other' && (file.realPath.startsWith(root + '/') || file.realPath.startsWith(root + '\\'))) {
    throw new Error('Checkout executable refused. Use the verified path printed by oberth join.')
  }
  return file.realPath
}

type Connection = { executable: string; profile: string }

async function connections($: EngineInterface, options: PluginOptions, source?: string): Promise<Connection[]> {
  const executable = await checkedExecutable($, options)
  const names = source ? [source] : await profiles($, options)
  return names.map(profile => ({ executable, profile }))
}

async function json($: EngineInterface, connection: Connection, args: string[]): Promise<unknown> {
  const argv = [connection.executable, ...args, '--json']
  const out = await $.process.run(argv, { timeoutMs: 3500, env: { OBERTH_PROFILE: connection.profile } })
  if (out.isStdoutTruncated || out.isStderrTruncated) throw new Error('Run output exceeded the host output limit.')
  if (out.exitCode !== 0) {
    throw new Error(out.stderr.trim() || out.stdout.trim() || 'Oberth is unreachable. Check the selected CLI profile.')
  }
  return JSON.parse(out.stdout)
}

// Read each selected profile, then merge before applying the total row limit.
export async function list($: EngineInterface, options: PluginOptions, count: number) {
  const peers = await connections($, options)
  const pages = await Promise.allSettled(peers.map(peer => listProfile($, peer, count)))
  const successful = pages.flatMap(page => page.status === 'fulfilled' ? [page.value] : [])
  const created = (run: ObservedRun) => Date.parse(run.created_at) + (run.clockOffset ?? 0)
  const runs = successful.flatMap(page => page.runs).sort((a, b) => created(b) - created(a))
  const unique = runs.filter((run, index) => runs.findIndex(row => row.id === run.id) === index)
  const unavailable = peers.filter((_, index) => pages[index]!.status === 'rejected').map(peer => peer.profile)
  const clocks = pages.flatMap((page, index) => page.status === 'fulfilled' ? [{ profile: peers[index]!.profile, offset: page.value.clockOffset }] : [])
  return { runs: unique.slice(0, count), clocks, unavailable }
}

async function listProfile($: EngineInterface, connection: Connection, count: number) {
  const page = await json($, connection, ['run', 'list', '--limit', String(count)]) as { runs?: unknown[] | null; server_time?: unknown }
  if (!page || !Object.hasOwn(page, 'runs')) throw new Error('Invalid run-list JSON.')
  if (page.runs !== null && !Array.isArray(page.runs)) throw new Error('Invalid run-list JSON.')
  const time = typeof page.server_time === 'string' ? Date.parse(page.server_time) : NaN
  const now = await $.clock.now()
  const clockOffset = Number.isFinite(time) ? now - time : 0
  return { clockOffset, runs: (page.runs ?? []).map(value => ({ ...decodeRun(value), profile: connection.profile, clockOffset })) }
}

export async function get($: EngineInterface, options: PluginOptions, id: string, source?: string): Promise<ObservedRun> {
  if (!id || id.startsWith('-') || /\s/.test(id)) throw new Error('Use /oberth-watch <run id or prefix>.')
  const peers = await connections($, options, source)
  const results = await Promise.allSettled(peers.map(async connection => ({
    ...decodeRun(await json($, connection, ['run', 'get', id])), profile: connection.profile,
  })))
  const runs = results.flatMap(result => result.status === 'fulfilled' ? [result.value] : [])
  if (!runs.length) throw (results[0] as PromiseRejectedResult).reason
  if (runs.some(run => run.id !== runs[0]!.id || run.repo !== runs[0]!.repo)) throw new Error('Run prefix matches several profiles. Use the full run ID.')
  return runs[0]!
}
