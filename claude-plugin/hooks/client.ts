import type { EngineInterface } from 'claude-code'
import type { Run, Step } from './format'

export const INSTALL = 'Oberth is not configured. Run /oberth:install.'
export class ReadError extends Error {
  constructor(public kind: 'setup' | 'missing' | 'unavailable') {
    super(kind === 'setup' ? INSTALL : kind === 'missing' ? 'Oberth run not found; watch stopped.' : 'Oberth is unavailable; try again later.')
  }
}

// Read the same user settings and saved endpoint as scripts/trust configure.
// Credential contents never enter this module; the CLI opens the selected files.
async function connection($: EngineInterface): Promise<string[]> {
  try {
    const home = await $.env.get('HOME')
    const config = await $.env.get('CLAUDE_CONFIG_DIR') || (home ? `${home}/.claude` : '')
    if (!config) throw new ReadError('setup')
    const url = (await $.fs.read(`${config}/oberth/url`)).trim()
    const settings = await $.settings.read({ source: 'user' }) as { env?: Record<string, string> }
    const cert = settings.env?.CLAUDE_CODE_CLIENT_CERT
    const key = settings.env?.CLAUDE_CODE_CLIENT_KEY
    const ca = `${config}/oberth/ca.crt`
    if (!url.startsWith('https://') || !cert || !key) throw new ReadError('setup')
    for (const file of [ca, cert, key]) {
      if ((await $.fs.stat(file)).kind !== 'file') throw new ReadError('setup')
    }
    let cli = 'oberth'
    try {
      if (home && (await $.fs.stat(`${home}/.local/bin/oberth`)).kind === 'file') cli = `${home}/.local/bin/oberth`
    } catch { /* A PATH installation is also supported. */ }
    return [cli, 'mcp', 'call', '--url', url, '--ca-file', ca, '--client-cert', cert,
      '--client-key', key, '--timeout', '10s', '--reconnect-window', '1ms', '--args-file', '-']
  } catch { throw new ReadError('setup') }
}

async function call($: EngineInterface, tool: 'run_list' | 'run_get', args: object): Promise<unknown> {
  const argv = await connection($)
  let out
  try {
    out = await $.process.run([...argv, tool], { stdin: JSON.stringify(args), timeoutMs: 12000 })
  } catch (err) {
    // A missing executable is a setup error; never show a host exception.
    const message = String(err).toLowerCase()
    throw new ReadError(/enoent|not found|executable|spawn/.test(message) ? 'setup' : 'unavailable')
  }
  if (out.exitCode !== 0) {
    const text = `${out.stdout} ${out.stderr}`.toLowerCase()
    if (/not found|not_found|no such run/.test(text)) throw new ReadError('missing')
    if (/credential|certificate|authentication|unauthorized|not configured/.test(text)) throw new ReadError('setup')
    throw new ReadError('unavailable')
  }
  try { return JSON.parse(out.stdout) } catch { throw new ReadError('unavailable') }
}

function record(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new ReadError('unavailable')
  return value as Record<string, unknown>
}
function run(value: unknown): Run {
  const r = record(value)
  if (typeof r.id !== 'string' || !r.id || typeof r.status !== 'string') throw new ReadError('unavailable')
  for (const key of ['repo', 'ref', 'display_status', 'failed_step', 'created_at', 'started_at', 'finished_at']) {
    if (r[key] != null && typeof r[key] !== 'string') throw new ReadError('unavailable')
  }
  return r as Run
}

export async function listRuns($: EngineInterface, count: number): Promise<Run[]> {
  const value = record(await call($, 'run_list', { limit: count }))
  if (!Array.isArray(value.runs)) throw new ReadError('unavailable')
  return value.runs.slice(0, count).map(run)
}
export async function getRun($: EngineInterface, id: string): Promise<Run> {
  const value = record(await call($, 'run_get', { id, summary: false }))
  const r = run(value.run)
  if (!r.repo && value.repository) {
    const repository = record(value.repository)
    if (typeof repository.name === 'string') r.repo = repository.name
  }
  const steps = value.steps ?? []
  if (!Array.isArray(steps)) throw new ReadError('unavailable')
  r.steps = steps.map(value => {
    const step = record(value)
    for (const key of ['step', 'status', 'started_at', 'finished_at']) {
      if (step[key] != null && typeof step[key] !== 'string') throw new ReadError('unavailable')
    }
    return step as Step
  })
  return r
}
