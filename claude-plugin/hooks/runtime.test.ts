import { expect, mock, test } from 'claude-code/testing'

test('registers eager read-only tools', async ($, on) => {
  mock.clock(on)
  const tools: string[] = []
  on('tool.register', (_, e) => {
    expect(e.isDeferred).toBe(false)
    tools.push(e.name)
    return { value: { tool: `mcp__oberth__${e.name}` } }
  })
  on('env.get', () => ({ value: undefined }))
  on('ui.status', () => ({ value: undefined }))
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  expect(tools).toEqual(['runs', 'watch'])
})

test('missing configuration prints one install instruction without invoking CLI', async ($, on) => {
  let calls = 0
  on('env.get', () => ({ value: undefined }))
  on('process.run', () => { calls++; throw new Error('must not execute') })
  const result = await $.command.run({ command: 'oberth:runs', args: '', origin: { kind: 'composer' }, presentation: { isFullscreen: false, columns: 120 } })
  expect(result.text).toBe('Oberth is not configured. Run /oberth:install.')
  expect(calls).toBe(0)
})

// The bottom process hook is the fake CLI. No host command or network runs.
function fixture(on: import('claude-code').On) {
  const clock = mock.clock(on, { now: Date.parse('2026-11-14T12:00:00Z') })
  mock.env(on, { HOME: '/test/home', CLAUDE_CONFIG_DIR: '/test/config' })
  on('settings.read', () => ({ value: { env: {
    CLAUDE_CODE_CLIENT_CERT: '/test/client.crt', CLAUDE_CODE_CLIENT_KEY: '/test/client.key',
  } } }))
  on('fs.read', (_, e) => {
    expect(e.path).toBe('/test/config/oberth/url')
    return { value: 'https://oberth.test/mcp\n' }
  })
  on('fs.stat', () => ({ value: { kind: 'file', size: 1, mtimeMs: 0, isLink: false } }))
  on('tool.register', (_, e) => ({ value: { tool: `mcp__oberth__${e.name}` } }))
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('session.end', (_, e) => ({ sessionId: e.sessionId }))
  const state = { status: 'running', missing: false, delay: 0, gets: 0, lists: 0, inFlight: 0, maxFlight: 0,
    cliMissing: false, invalidJSON: false, toasts: [] as string[], statuses: [] as string[], limits: [] as number[] }
  on('ui.status', (_, e) => { state.statuses.push(e.text ?? ''); return { value: undefined } })
  on('ui.toast', (_, e) => { state.toasts.push(e.text); return { value: undefined } })
  on('process.run', async (_, e) => {
    expect(e.argv[0]).toBe('/test/home/.local/bin/oberth')
    expect(e.argv).toContain('--client-cert')
    expect(e.argv).toContain('/test/client.crt')
    expect(e.argv).toContain('--client-key')
    expect(e.argv).toContain('/test/client.key')
    expect(e.argv).not.toContain('--token-file')
    expect(e.init?.timeoutMs).toBe(12000)
    expect(e.argv).toContain('10s')
    if (state.cliMissing) return { deny: 'spawn ENOENT' }
    const tool = e.argv[e.argv.length - 1]
    expect(['run_list', 'run_get']).toContain(tool!)
    state.inFlight++
    state.maxFlight = Math.max(state.maxFlight, state.inFlight)
    if (state.delay) await clock.sleep(state.delay)
    state.inFlight--
    const args = JSON.parse(e.init?.stdin || '{}')
    const run = { id: '1234567890', repo: 'oberth', ref: 'main', status: state.status, created_at: '2026-11-14T11:00:00Z' }
    let value
    if (tool === 'run_list') {
      state.lists++
      state.limits.push(args.limit)
      value = { runs: [run] }
    } else {
      state.gets++
      expect(args.id).toBe('1234567890')
      expect(args.summary).toBe(false)
      value = { run: { ...run, repo: '' }, repository: { name: 'oberth' }, steps: [{ step: 'unit', status: state.status, started_at: '2026-11-14T11:00:00Z',
        finished_at: state.status === 'passed' ? '2026-11-14T11:03:00Z' : null }] }
    }
    return { value: { exitCode: state.missing && tool === 'run_get' ? 1 : 0,
      stdout: state.missing && tool === 'run_get' ? 'run not found' : state.invalidJSON ? '{bad' : JSON.stringify(value),
      stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  return { clock, state }
}

const command = (name: string, args = '') => ({ command: `oberth:${name}`, args,
  origin: { kind: 'composer' as const }, presentation: { isFullscreen: false, columns: 120 } })
const band = { plugin: 'oberth', component: 'AbovePrompt' as const, props: {
  hasSurvey: false, isCollapsed: false, isWorking: false, maxRows: 8, bodyColumns: 100,
  scroll: { offset: 0, bodyRows: 8 }, view: {},
} }

test('latest watch refreshes every 15 seconds, toasts once and stops at completion', async ($, on) => {
  const { clock, state } = fixture(on)
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.settle()
  expect((await $.command.run(command('watch'))).text).toContain('Watching 12345678')
  expect(state.gets).toBe(1)
  await clock.advance(14999)
  expect(state.gets).toBe(1)
  state.status = 'passed'
  await clock.advance(1)
  expect(state.gets).toBe(2)
  expect(state.toasts.length).toBe(1)
  expect(state.toasts[0]).toContain('3m')
  await clock.advance(60000)
  expect(state.gets).toBe(2)
  expect(state.toasts.length).toBe(1)
  expect(state.statuses[state.statuses.length - 1]).toBe('Oberth ▸ 0 running · last ✓ main')
  for (const surface of ['terminal', 'desktop'] as const) {
    const ui = await $.ui.mount({ ...band, surface })
    expect((await ui.find({ type: 'Text', text: /12345678/ }))?.text).toContain('3m')
    await ui.unmount()
  }
})

test('a missing watched run stops polling and clears the live line', async ($, on) => {
  const { clock, state } = fixture(on)
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.settle()
  await $.command.run(command('watch', '1234567890'))
  state.missing = true
  await clock.advance(15000)
  const gets = state.gets
  await clock.advance(45000)
  expect(state.gets).toBe(gets)
  expect(state.toasts).toEqual(['Oberth run not found; watch stopped.'])
  const ui = await $.ui.mount({ ...band, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /12345678/ })).toBeUndefined()
  await ui.unmount()
})

test('slow CLI refreshes never overlap and session end discards their results', async ($, on) => {
  const { clock, state } = fixture(on)
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.settle()
  await $.command.run(command('watch', '1234567890'))
  state.delay = 40000
  await clock.advance(45000)
  expect(state.inFlight).toBe(1)
  expect(state.maxFlight).toBe(1)
  await $.session.end({ reason: 'clear', sessionId: 'test', resume: { id: 'test' } })
  const lists = state.lists
  await clock.advance(120000)
  expect(state.inFlight).toBe(0)
  expect(state.lists).toBe(lists)
  expect(state.toasts.length).toBe(0)
  const ui = await $.ui.mount({ ...band, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /12345678/ })).toBeUndefined()
  await ui.unmount()
})

test('CLI unavailable and malformed responses remain single safe lines', async ($, on) => {
  const { state } = fixture(on)
  state.cliMissing = true
  expect((await $.command.run(command('runs'))).text).toBe('Oberth is not configured. Run /oberth:install.')
  state.cliMissing = false
  state.invalidJSON = true
  expect((await $.command.run(command('runs'))).text).toBe('Oberth is unavailable; try again later.')
})

test('command and tool tables honor the limit and both surfaces draw every row', async ($, on) => {
  const { state } = fixture(on)
  const output = await $.command.run(command('runs', '--limit=3'))
  expect(state.limits).toEqual([3])
  expect(output.text).toContain('FAILED STEP')
  const tool = await $.tool.call({ tool: 'mcp__oberth__runs', limit: 2 })
  expect(state.limits).toEqual([3, 2])
  expect(tool.result).toBe(output.text)
  for (const surface of ['terminal', 'desktop'] as const) {
    const ui = await $.ui.mount({ plugin: 'oberth', component: 'CommandOutput', surface,
      props: { command: 'oberth:runs', args: '--limit=3', text: output.text || '', isErrored: false } })
    expect((await ui.find({ type: 'Text', text: /FAILED STEP/ }))?.text).toContain('FAILED STEP')
    expect((await ui.find({ type: 'Text', text: /12345678/ }))?.text).toContain('oberth')
    await ui.unmount()
    const result = await $.ui.mount({ plugin: 'oberth', component: 'ToolResult', surface,
      props: { tool_use_id: 'test', tool: 'mcp__oberth__runs', output: tool.result, isErrored: false } })
    expect((await result.find({ type: 'Text', text: /12345678/ }))?.text).toContain('oberth')
    await result.unmount()
  }
})


test('clear stops timers until an explicit watch in the new conversation', async ($, on) => {
  const { clock, state } = fixture(on)
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.settle()
  await $.command.run(command('watch', '1234567890'))
  await $.session.end({ reason: 'clear', sessionId: 'test', resume: { id: 'test' } })
  const gets = state.gets
  const lists = state.lists
  await clock.advance(30000)
  expect(state.gets).toBe(gets)
  expect(state.lists).toBe(lists)
  const result = await $.tool.call({ tool: 'mcp__oberth__watch' })
  expect(result.result).toContain('Watching 12345678')
  await clock.advance(15000)
  expect(state.gets).toBe(gets + 2)
})
