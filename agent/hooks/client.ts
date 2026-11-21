import type { EngineInterface, PluginOptions } from 'claude-code'
import { decodeRun } from './model'

export const SETUP = 'Set executable to the absolute verified path printed by oberth join.'
export const PROFILE_SETUP = 'Set profile to the uplink profile in the agent tools line from oberth join, or set OBERTH_PROFILE.'

export async function profile($: EngineInterface, options: PluginOptions): Promise<string> {
  const name = options.profile || await $.env.get('OBERTH_PROFILE')
  if (typeof name !== 'string' || !name.trim()) throw new Error(PROFILE_SETUP)
  return name
}

export function executable(options: PluginOptions): string {
  const path = options.executable
  if (typeof path !== 'string' || !/^(\/|[A-Za-z]:[\\/]|\\\\)/.test(path) || path.includes('\0')) {
    throw new Error(SETUP)
  }
  return path
}

async function checkedExecutable($: EngineInterface, options: PluginOptions): Promise<string> {
  const path = executable(options)
  const cwd = await $.session.root()
  const [file, directory, checkout] = await Promise.all([
    $.fs.stat(path, { resolve: true }), $.fs.stat(cwd, { resolve: true }),
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

export async function list($: EngineInterface, options: PluginOptions, count: number) {
  const page = await json($, options, ['run', 'list', '--limit', String(count)]) as { runs?: unknown[] | null }
  if (!page || !Object.hasOwn(page, 'runs')) throw new Error('Invalid run-list JSON.')
  if (page.runs === null) return []
  if (!Array.isArray(page.runs)) throw new Error('Invalid run-list JSON.')
  return page.runs.map(decodeRun)
}

export async function get($: EngineInterface, options: PluginOptions, id: string) {
  if (!id || id.startsWith('-') || /\s/.test(id)) throw new Error('Use /oberth-watch <run id or prefix>.')
  return decodeRun(await json($, options, ['run', 'get', id]))
}
