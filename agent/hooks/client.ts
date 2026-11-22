import type { EngineInterface, PluginOptions } from 'claude-code'
import { decodeRun } from './model'

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

async function json($: EngineInterface, options: PluginOptions, args: string[]): Promise<unknown> {
  const argv = [await checkedExecutable($, options), ...args, '--json']
  const selected = await profile($, options)
  const out = await $.process.run(argv, { timeoutMs: 3500, env: { OBERTH_PROFILE: selected } })
  if (out.isStdoutTruncated || out.isStderrTruncated) throw new Error('Run output exceeded the host output limit.')
  if (out.exitCode !== 0) {
    throw new Error(out.stderr.trim() || out.stdout.trim() || 'Oberth is unreachable. Check the selected CLI profile.')
  }
  return JSON.parse(out.stdout)
}

// The listed runs and the server's clock when it read them (undefined from a server
// that does not send it).
export async function list($: EngineInterface, options: PluginOptions, count: number) {
  const page = await json($, options, ['run', 'list', '--limit', String(count)]) as { runs?: unknown[] | null; server_time?: unknown }
  if (!page || !Object.hasOwn(page, 'runs')) throw new Error('Invalid run-list JSON.')
  if (page.runs !== null && !Array.isArray(page.runs)) throw new Error('Invalid run-list JSON.')
  const time = typeof page.server_time === 'string' ? Date.parse(page.server_time) : NaN
  return { runs: (page.runs ?? []).map(decodeRun), serverTime: Number.isFinite(time) ? time : undefined }
}

export async function get($: EngineInterface, options: PluginOptions, id: string) {
  if (!id || id.startsWith('-') || /\s/.test(id)) throw new Error('Use /oberth-watch <run id or prefix>.')
  return decodeRun(await json($, options, ['run', 'get', id]))
}
