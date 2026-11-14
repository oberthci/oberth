import { expect, test } from 'claude-code/testing'
import type { EngineInterface } from 'claude-code'
import { getRun, INSTALL, INVALID_ID, listRuns, UNAVAILABLE } from './client'

// Direct engine double lets process.run reject as the real spawn does. A hook
// returning {deny} would exercise hook refusal, not a failed host spawn.
function client() {
  const home = '/test/user'
  const installed = `${home}/.local/bin/oberth`
  const state = { installed, env: { HOME: home, CLAUDE_CONFIG_DIR: '/test/config', PATH: '/test/bin' } as Record<string, string>,
    files: new Set([installed, '/test/config/oberth/ca.crt', '/test/cert', '/test/key']),
    cert: true, calls: [] as string[][], stats: [] as string[], reject: false, disappearing: false,
    exit: 0, stdout: '{"runs":[]}', stderr: '', now: 0 }
  const engine = {
    env: { get: async (key: string) => state.env[key] },
    settings: { read: async () => ({ env: state.cert ? {
      CLAUDE_CODE_CLIENT_CERT: '/test/cert', CLAUDE_CODE_CLIENT_KEY: '/test/key',
    } : { OBERTH_TOKEN_FILE: '/test/token' } }) },
    clock: { now: async () => state.now },
    fs: { read: async () => 'https://oberth.test/mcp', stat: async (path: string) => {
      state.stats.push(path)
      if (!state.files.has(path)) throw new Error('ENOENT')
      return { kind: 'file' }
    } },
    process: { run: async (argv: string[]) => {
      state.calls.push(argv)
      if (state.disappearing) state.files.delete(argv[0]!)
      if (state.reject || state.disappearing) throw new Error('spawn ENOENT executable certificate not found')
      return { exitCode: state.exit, stdout: state.stdout, stderr: state.stderr, isStdoutTruncated: false, isStderrTruncated: false }
    } },
  } as unknown as EngineInterface
  return { engine, state }
}

test('CLI lookup prefers the verified home install then only absolute PATH entries', async () => {
  const { engine, state } = client()
  state.env.PATH = '.:relative::/test/bin'
  state.files.add('/test/bin/oberth')
  await listRuns(engine, 5)
  expect(state.calls[0]![0]).toBe(state.installed)
  state.files.delete(state.installed)
  await listRuns(engine, 5)
  expect(state.calls[1]![0]).toBe('/test/bin/oberth')
  expect(state.stats).not.toContain('./oberth')
  expect(state.stats).not.toContain('relative/oberth')
  expect(state.stats).not.toContain('/oberth')
  state.files.delete('/test/bin/oberth')
  await expect(listRuns(engine, 5)).rejects.toThrow(INSTALL)
  expect(state.calls.length).toBe(2)
})

test('missing CLI rejects like a real failed spawn and timeout wording never decides setup', async () => {
  const { engine, state } = client()
  state.disappearing = true
  await expect(listRuns(engine, 5)).rejects.toThrow(INSTALL)
  expect(state.calls.length).toBe(1)
  state.disappearing = false
  state.files.add(state.installed)
  state.reject = true
  await expect(listRuns(engine, 5)).rejects.toThrow(UNAVAILABLE)
})

test('token-only configuration names the required certificate view without claiming unconfigured', async () => {
  const { engine, state } = client()
  state.cert = false
  await expect(listRuns(engine, 5)).rejects.toThrow(INSTALL)
  expect(INSTALL).not.toContain('not configured')
  expect(state.calls.length).toBe(0)
})

test('only the exact run_get tool-failure envelope means a missing run', async () => {
  const { engine, state } = client()
  state.exit = 1
  for (const stderr of ['oberth: server error: method not found (code -32601)', 'oberth: server returned HTTP 404: not found',
    'certificate authentication failed', 'network unavailable']) {
    state.stderr = stderr
    state.stdout = 'not found'
    await expect(getRun(engine, '12345678')).rejects.toThrow(UNAVAILABLE)
  }
  state.stderr = 'oberth: tool returned an error\n'
  state.stdout = 'not found\n'
  await expect(getRun(engine, '12345678')).rejects.toThrow('Oberth run not found.')
  await expect(listRuns(engine, 5)).rejects.toThrow(UNAVAILABLE)
  state.exit = 2
  await expect(getRun(engine, '12345678')).rejects.toThrow(UNAVAILABLE)
})

test('invalid run ids never invoke the CLI and malformed list rows do not hide good rows', async () => {
  const { engine, state } = client()
  for (const id of ['abcdefg1', 'abc', 'a'.repeat(33), '12345678 --off']) {
    await expect(getRun(engine, id)).rejects.toThrow(INVALID_ID)
  }
  expect(state.calls.length).toBe(0)
  state.stdout = '{"runs":[null,{"id":"12345678","status":"queued"},{"id":"bad","status":12}]}'
  expect((await listRuns(engine, 5)).map(row => row.id)).toEqual(['12345678'])
})
