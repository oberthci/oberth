import type { EngineInterface } from 'claude-code'
import { clip } from './format'
import type { Run, Step } from './format'

export const INSTALL = 'This Oberth view needs client-certificate setup. Run /oberth:install.'
export const UNAVAILABLE = 'unavailable: CLI request failed.'
export const VALID_ID = /^[0-9a-f]{8,32}$/i
export const INVALID_ID = 'Run id must be 8 to 32 hexadecimal characters.'

function errorLine(detail: string): string {
  const first = detail.split(/[\r\n]/, 1)[0] || 'CLI request failed.'
  return `unavailable: ${clip(first, 67)}`
}

export class ReadError extends Error {
  constructor(public kind: 'setup' | 'unavailable' | 'spawn' | 'timeout' | 'invalid', detail = 'CLI request failed.') {
    super(kind === 'setup' ? INSTALL : kind === 'invalid' ? INVALID_ID : errorLine(detail))
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
    const cli = await executable($, home)
    return [cli, 'mcp', 'call', '--url', url, '--ca-file', ca, '--client-cert', cert,
      '--client-key', key, '--timeout', '10s', '--reconnect-window', '1ms', '--args-file', '-']
  } catch { throw new ReadError('setup') }
}

// Never let the project directory or a relative/empty PATH entry choose code.
async function executable($: EngineInterface, home: string | undefined): Promise<string> {
  const candidates = home?.startsWith('/') ? [`${home}/.local/bin/oberth`] : []
  for (const entry of (await $.env.get('PATH') || '').split(':')) {
    if (entry.startsWith('/')) candidates.push(`${entry}/oberth`)
  }
  for (const candidate of candidates) {
    try { if ((await $.fs.stat(candidate)).kind === 'file') return candidate } catch { /* Try the next absolute candidate. */ }
  }
  throw new ReadError('setup')
}

async function call($: EngineInterface, tool: 'run_list' | 'run_get', args: object, deadline?: number): Promise<unknown> {
  const argv = await connection($)
  const remaining = deadline === undefined ? 12000 : Math.min(12000, deadline - await $.clock.now())
  if (remaining <= 0) throw new ReadError('timeout', 'CLI request timed out.')
  let out
  try {
    out = await $.process.run([...argv, tool], { stdin: JSON.stringify(args), timeoutMs: remaining })
  } catch (error) {
    // Only structured exception fields distinguish spawn/timeout failures.
    // The engine may omit them; an untyped rejection remains unavailable.
    const failure = error as { code?: unknown; name?: unknown } | null
    if (failure?.code === 'ETIMEDOUT' || failure?.name === 'TimeoutError') {
      throw new ReadError('timeout', 'CLI request timed out.')
    }
    if (failure?.code === 'ENOENT' || failure?.code === 'EACCES' || failure?.code === 'ENOEXEC') {
      throw new ReadError('spawn', 'CLI could not start.')
    }
    throw new ReadError('unavailable')
  }
  if (out.isStdoutTruncated || out.isStderrTruncated) throw new ReadError('unavailable', 'CLI output was truncated.')
  if (out.exitCode !== 0) throw new ReadError('unavailable', out.stdout || out.stderr)
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

export async function listRuns($: EngineInterface, count: number, deadline?: number): Promise<Run[]> {
  const value = record(await call($, 'run_list', { limit: count }, deadline))
  if (!Array.isArray(value.runs)) throw new ReadError('unavailable')
  // One malformed row must not hide every readable run; retain all active rows.
  return value.runs.flatMap(value => { try { return [run(value)] } catch { return [] } })
}
export async function getRun($: EngineInterface, id: string, deadline?: number): Promise<Run> {
  if (!VALID_ID.test(id)) throw new ReadError('invalid')
  const value = record(await call($, 'run_get', { id, summary: false }, deadline))
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
