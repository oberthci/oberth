// Rendering is a manual gate: run `claude plugin test ./agent`; the build image has no host JavaScript runtime.
import { expect, mock, test } from 'claude-code/testing'
import type { Engine } from 'claude-code/testing'
import type { On } from 'claude-code'
import type { Run, Snapshot } from '../types'
import { age, boxRows, decodeRun, duration, progress, PROFILE_REASON, PROFILE_REMEDY, ASSISTED_REASON, REFRESH_MS, RESUMED_REASON, shortID, shown, SHOWN_REASON, SHOWN_REMEDY, span, table, visible, WINDOW_MS } from './model'
import { SETUP } from './client'
import { hostProps, hostValues } from './host-elements'

// Check the floor contract even when the current harness accepts newer values.
function validateTree(node: unknown): void {
  if (typeof node === 'string') return
  const element = node as { type: keyof typeof hostProps; props?: Record<string, unknown>; children?: unknown[] }
  const allowed: readonly string[] | undefined = hostProps[element.type]
  const values: Record<string, readonly unknown[]> = hostValues[element.type]
  if (!allowed) throw new Error(`Unknown element ${element.type}`)
  for (const [prop, value] of Object.entries(element.props ?? {})) {
    if (!allowed.includes(prop)) throw new Error(`${element.type} prop ${prop} is not allowed`)
    if (values[prop] && !values[prop]!.includes(value)) throw new Error(`${element.type} ${prop} value is not allowed`)
  }
  for (const child of element.children ?? []) validateTree(child)
}

// The default run's row: id, short repository, short ref, then the status word beside its icon.
const ROW = /^cad7d9ec oberth +v0\.24\.0 +▶ running /
const HEADER = /^RUN +REPO +REF +STATUS +STEPS +TIME +AGE$/

const options = { executable: '/verified/oberth', profile: 'joined-uplink' }
const commandInput = { origin: { kind: 'plugin', name: 'oberth' },
  presentation: { isFullscreen: false, columns: 100 } } as const
const start = { cwd: '/checkout', surface: 'terminal', isInteractive: true } as const
const band = {
  plugin: 'oberth', component: 'AbovePrompt',
  props: { hasSurvey: false, isCollapsed: false, isWorking: false, maxRows: 20,
    bodyColumns: 100, scroll: { offset: 0, bodyRows: 20 }, view: {} },
} as const

function run(id = 'cad7d9ec000000000000000000000000', status: Run['status'] = 'running'): Run {
  return { id, repo: 'oberth', ref: 'v0.24.0', status, display_reason: '',
    created_at: '2026-11-18T00:00:00Z', started_at: '2026-11-18T00:00:00Z', finished_at: null,
    step_count: 11, steps_planned_exact: true, passed: 6, failed: 0, skipped: 0 }
}

// Hooks registered first sit outermost, and none may be added after the first `$` call:
// the snapshot observer is registered here, before any state hook a test adds beneath it.
const writes = new WeakMap<On, () => Promise<void>>()

// host: HOME and a path whose stat fails.
type Host = { home?: string; missing?: string }
// Model tools the plugin asked the host for, per test; the plugin must ask for none.
const registeredTools = new WeakMap<On, string[]>()

function bottom(on: On, resolved?: string, checkout = true, profileEnv?: string, host: Host = {}) {
  on('env.get', (_, e) => ({ value: e.name === 'OBERTH_PROFILE' ? profileEnv : e.name === 'HOME' ? host.home : undefined }))
  const waiting: (() => void)[] = []
  on('state.set', { plugin: 'oberth', key: 'snapshot' }, async (_, e, next) => {
    const result = await next(e)
    for (const resolve of waiting.splice(0)) resolve()
    return result
  })
  writes.set(on, () => new Promise(resolve => { waiting.push(resolve) }))
  const store = new Map<string, unknown>()
  on('store.get', (_, e) => ({ value: store.get(e.key) }))
  on('store.set', (_, e) => { store.set(e.key, e.value); return { value: undefined } })
  on('command.register', (_, e) => ({ value: { command: e.name } }))
  const tools: string[] = []
  registeredTools.set(on, tools)
  on('tool.register', (_, e) => { tools.push(e.name); return { deny: 'MCP server "oberth" is the joined bridge.' } })
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('session.end', () => ({ sessionId: 'ended' }))
  on('session.root', () => ({ value: '/checkout' }))
  on('ui.render', { component: 'AbovePrompt' }, ($, e) => {
    const { Box } = $.ui.resolve(e)
    return <Box />
  })
  on('fs.stat', (_, e) => {
    if (e.path.endsWith('/.git') && !checkout || e.path === host.missing) throw new Error('not found')
    return { value: {
      kind: e.path === '/checkout' ? 'dir' : 'file',
      realPath: e.path === '/checkout' ? e.path : resolved ?? e.path,
      size: 1, mtimeMs: 0, isLink: false,
    } }
  })
  return store
}

function nextSnapshot(on: On): Promise<void> {
  return writes.get(on)!()
}

async function startSession($: Engine, on: On) {
  const refreshed = nextSnapshot(on)
  await $.session.start(start)
  await refreshed
}

async function text($: Engine, expression: RegExp) {
  for (const surface of ['terminal', 'desktop', 'mobile'] as const) {
    const ui = await $.ui.mount({ ...band, surface })
    expect((await ui.find({ type: 'Text', text: expression })) !== undefined).toBe(true)
    await ui.unmount()
  }
}

test('unset executable refuses both commands without spawning', async ($, on) => {
  bottom(on)
  let stored: { value: { shape: string; value: Snapshot }; version: number } = {
    value: { shape: 'runs-1', value: { recent: [run()], pins: [], reachable: true, now: 0, singleProfile: false } }, version: 1,
  }
  on('state.get', { plugin: 'oberth', key: 'snapshot' }, () => ({ value: stored }))
  on('state.set', { plugin: 'oberth', key: 'snapshot' }, (_, e) => {
    stored = { value: e.value, version: stored.version + 1 }
    return { value: { isSet: true, version: stored.version } }
  })
  const calls: unknown[] = []
  on('process.run', (_, e) => { calls.push(e); throw new Error('must not spawn') })
  const toasts: string[] = []
  on('ui.toast', (_, e) => { toasts.push(e.text); return { value: undefined } })
  await text($, ROW)
  await startSession($, on)
  await startSession($, on)
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text).toContain('absolute verified path')
  expect((await $.command.run({ ...commandInput, command: 'oberth-watch', args: run().id })).text).toContain('absolute verified path')
  expect(calls.length).toBe(0)
  expect(toasts.length).toBe(1)
  const ui = await $.ui.mount({ ...band, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /cad7d9ec/ })).toBeUndefined()
  await ui.unmount()
})

test('session start does not wait for the first refresh and slow reads coalesce', { options }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on)
  let release!: () => void
  const blocked = new Promise<void>(resolve => { release = resolve })
  let calls = 0
  on('process.run', async (_, e) => {
    calls++
    expect(e.init?.timeoutMs).toBe(3500)
    await blocked
    return { value: { exitCode: 0, stdout: JSON.stringify({ runs: [run()] }),
      stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  await $.session.start(start)
  await clock.advance(60000)
  expect(calls).toBe(1)
  const ui = await $.ui.mount({ ...band, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /cad7d9ec/ })).toBeUndefined()
  const refreshed = nextSnapshot(on)
  release()
  await refreshed
  await ui.unmount()
  await text($, ROW)
})

test('relative executable refuses before process execution', { options: { executable: './oberth' } }, async ($, on) => {
  bottom(on)
  on('ui.toast', () => ({ value: undefined }))
  on('process.run', () => { throw new Error('must not spawn') })
  await startSession($, on)
  expect((await $.command.run({ ...commandInput, command: 'oberth-watch', args: 'cad7d9ec' })).text).toContain('absolute verified path')
})

test('unset profile without a checkout binding stays hidden, notifies once and never runs the CLI', { options: { executable: options.executable } }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on)
  const toasts: string[] = []
  on('ui.toast', (_, e) => { toasts.push(e.text); return { value: undefined } })
  on('process.run', (_, e) => {
    if (e.argv[0] === 'git') return { value: { exitCode: 1, stdout: '', stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
    throw new Error('must not spawn with the current profile')
  })
  await startSession($, on)
  await startSession($, on)
  await clock.advance(120000)
  expect(toasts.length).toBe(1)
  expect(toasts[0]).toContain('Set profile to the uplink profile')
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text).toContain('OBERTH_PROFILE')
  expect((await $.command.run({ ...commandInput, command: 'oberth-watch', args: run().id })).text).toContain('OBERTH_PROFILE')
  const ui = await $.ui.mount({ ...band, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /cad7d9ec/ })).toBeUndefined()
  await ui.unmount()
})

for (const explicit of [true, false]) {
  test(`profile selection uses ${explicit ? 'the plugin option' : 'OBERTH_PROFILE'} on every call`,
    { options: explicit ? options : { executable: options.executable } }, async ($, on) => {
      bottom(on, undefined, true, explicit ? 'other-profile' : 'joined-uplink')
      mock.clock(on)
      const profiles: unknown[] = []
      on('process.run', (_, e) => {
        profiles.push(e.init?.env)
        return { value: { exitCode: 0, stdout: JSON.stringify(e.argv[2] === 'get' ? run() : { runs: [] }),
          stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
      })
      await startSession($, on)
      await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })
      await $.command.run({ ...commandInput, command: 'oberth-watch', args: run().id })
      expect(profiles.length).toBe(3)
      expect(profiles).toEqual(Array(3).fill({ OBERTH_PROFILE: 'joined-uplink' }))
    })
}

async function boxLines($: Engine, surface: 'terminal' | 'desktop' | 'mobile' = 'terminal', bodyColumns = 100) {
  const ui = await $.ui.mount({ ...band, props: { ...band.props, bodyColumns }, surface })
  validateTree(await ui.drawn())
  const lines = (await ui.findAll({ type: 'Text' })).map(line => line.text)
  await ui.unmount()
  return lines
}

test('a finished run stays in place as passed for ten minutes, then the box goes', { options }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on, { now: Date.parse('2026-11-18T00:01:00Z') })
  const calls: string[][] = []
  const older = { ...run('a4c399fd000000000000000000000000', 'passed'), created_at: '2026-11-17T22:00:00Z',
    started_at: '2026-11-17T22:00:00Z', finished_at: '2026-11-17T23:00:00Z' }
  let rows = [run(), older]
  on('process.run', (_, e) => {
    if (e.argv[2] === 'list') calls.push([...e.argv])
    return { value: { exitCode: 0, stdout: JSON.stringify({ runs: rows }), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  await startSession($, on)
  expect(calls[0]).toEqual(['/verified/oberth', 'run', 'list', '--limit', '3', '--json'])
  for (const surface of ['terminal', 'desktop', 'mobile'] as const) {
    const lines = await boxLines($, surface)
    expect(lines).toHaveLength(3)
    expect(HEADER.test(lines[0]!)).toBe(true)
    expect(lines[1]).toMatch(/^cad7d9ec oberth +v0\.24\.0 +▶ running +6\/11 +1m00s +1m$/)
    expect(lines[2]).toMatch(/^a4c399fd oberth +v0\.24\.0 +✓ passed +6\/11 +1h00m +2h$/)
  }
  await clock.advance(60000)
  expect(calls.length).toBe(5)
  rows = [{ ...run(), status: 'passed', passed: 11, finished_at: '2026-11-18T00:02:00Z' }, older]
  await clock.advance(15000)
  const passed = await boxLines($)
  expect(passed[1]).toMatch(/^cad7d9ec oberth +v0\.24\.0 +✓ passed +11\/11 +2m00s +2m$/)
  expect(passed[0]!.indexOf('STATUS')).toBe(passed[1]!.indexOf('✓'))
  await clock.advance(WINDOW_MS - 30000)
  expect((await boxLines($))[1]).toMatch(/^cad7d9ec .*✓ passed .* 11m$/)
  await clock.advance(REFRESH_MS)
  await noBox($)
  const before = calls.length
  await clock.advance(REFRESH_MS - 1)
  expect(calls.length - before).toBe(0)
  await clock.advance(1)
  expect(calls.length - before).toBe(1)
  rows = [run(), older]
  await clock.advance(REFRESH_MS)
  await text($, ROW)
})

test('two runs list newest first and keep their places as the newer one finishes', { options }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on, { now: Date.parse('2026-11-18T00:05:00Z') })
  const newer = { ...run('e3d4dd29000000000000000000000000'), ref: 'refs/heads/smoke-2', created_at: '2026-11-18T00:04:00Z', started_at: '2026-11-18T00:04:00Z' }
  const first = { ...run('f366936d000000000000000000000000'), ref: 'refs/heads/smoke-1' }
  let rows = [newer, first]
  cli(on, () => rows)
  await startSession($, on)
  expect((await boxLines($)).slice(1).map(line => line.slice(0, 8))).toEqual(['e3d4dd29', 'f366936d'])
  rows = [{ ...newer, status: 'passed', finished_at: '2026-11-18T00:05:10Z' }, first]
  await clock.advance(REFRESH_MS)
  const lines = await boxLines($)
  expect(lines[1]).toMatch(/^e3d4dd29 oberth +smoke-2 +✓ passed +6\/11 +1m10s +1m$/)
  expect(lines[2]).toMatch(/^f366936d oberth +smoke-1 +▶ running +6\/11 +5m15s +5m$/)
})

for (const [name, skew] of [['11 minutes fast', 11 * 60000], ['30 minutes slow', -30 * 60000]] as const) {
  test(`with this host's clock ${name}, rows age and leave on the server's clock`, { options }, async ($, on) => {
    bottom(on)
    const local = Date.parse('2026-11-18T00:01:00Z')
    let server = local - skew
    const clock = mock.clock(on, { now: local })
    const iso = (time: number) => new Date(time).toISOString()
    let rows = [{ ...run(), created_at: iso(server - 60000), started_at: iso(server - 60000) }]
    on('process.run', () => ({ value: { exitCode: 0, stdout: JSON.stringify({ runs: rows, server_time: iso(server) }),
      stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }))
    const advance = async (ms: number) => { server += ms; await clock.advance(ms) }
    await startSession($, on)
    expect((await boxLines($))[1]).toMatch(/ ▶ running +6\/11 +1m00s +1m$/)
    rows = [{ ...rows[0]!, status: 'passed', finished_at: iso(server + REFRESH_MS) }]
    await advance(REFRESH_MS)
    expect((await boxLines($))[1]).toMatch(/ ✓ passed +6\/11 +1m15s +1m$/)
    await advance(WINDOW_MS - REFRESH_MS)
    expect((await boxLines($))[1]).toMatch(/ ✓ passed .* 11m$/)
    expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text).toMatch(/ ✓ passed .* 11m$/)
    await advance(REFRESH_MS)
    await noBox($)
  })
}

test('a watched run keeps the first row as it finishes behind three newer runs', { options }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on, { now: Date.parse('2026-11-18T00:01:00Z') })
  const newer = ['c1', 'c2', 'c3'].map(prefix => run(prefix.repeat(4) + '0'.repeat(24)))
  let watched = run('dddddddd000000000000000000000000')
  on('process.run', (_, e) => ({ value: { exitCode: 0, stdout: JSON.stringify(e.argv[2] === 'get' ? watched : { runs: newer }),
    stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }))
  await startSession($, on)
  await $.command.run({ ...commandInput, command: 'oberth-watch', args: watched.id })
  await clock.advance(REFRESH_MS)
  expect((await boxLines($)).slice(1).map(line => line.slice(0, 8))).toEqual(['dddddddd', 'c1c1c1c1', 'c2c2c2c2'])
  watched = { ...watched, status: 'passed', finished_at: '2026-11-18T00:01:20Z' }
  await clock.advance(REFRESH_MS)
  const lines = await boxLines($)
  expect(lines.slice(1).map(line => line.slice(0, 8))).toEqual(['dddddddd', 'c1c1c1c1', 'c2c2c2c2'])
  expect(lines[1]).toMatch(/^dddddddd .*✓ passed .*●$/)
})

// Terminal cells as the test reads them: CJK ideographs and the one emoji used take two.
const testCells = (line: string) => Array.from(line).reduce((sum, point) => sum + (/[一-鿿]|\u{1F600}/u.test(point) ? 2 : 1), 0)
const loneSurrogate = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/

test('wide characters count two cells and a cut never splits a code point', { options }, async ($, on) => {
  bottom(on)
  mock.clock(on, { now: Date.parse('2026-11-18T00:01:00Z') })
  const wide = { ...run('aaaaaaaa000000000000000000000000', 'failed'), ref: 'refs/heads/' + '分支名称'.repeat(5),
    display_reason: 'exited with code 2', failure: lintFailure }
  const emoji = { ...run(), ref: 'refs/heads/ab\u{1F600}cdefgh' }
  cli(on, () => [emoji, wide])
  await startSession($, on)
  for (const columns of [100, 80, 60]) {
    const text = (await $.command.run({ ...commandInput, presentation: { isFullscreen: false, columns }, command: 'oberth-runs', args: '' })).text!
    for (const line of text.split('\n')) expect(testCells(line)).toBeLessThanOrEqual(columns)
    expect(text).not.toMatch(loneSurrogate)
    const lines = text.split('\n')
    expect(testCells(lines[1]!.slice(0, lines[1]!.indexOf('▶')))).toBe(testCells(lines[2]!.slice(0, lines[2]!.indexOf('✗'))))
  }
  const narrow = (await $.command.run({ ...commandInput, presentation: { isFullscreen: false, columns: 60 }, command: 'oberth-runs', args: '' })).text!
  expect(narrow.split('\n')[1]).toContain(' ab… ')
  for (const line of await boxLines($, 'terminal', 50)) {
    expect(testCells(line)).toBeLessThanOrEqual(46)
    expect(line).not.toMatch(loneSurrogate)
  }
})

test('rows stay single aligned lines within narrow widths', { options }, async ($, on) => {
  bottom(on)
  mock.clock(on, { now: Date.parse('2026-11-18T00:01:00Z') })
  const rows = [
    { ...run(), repo: 'origin/team/a-repository-name-that-exceeds-every-narrow-column', ref: 'refs/heads/topic' },
    { ...run('aaaaaaaa000000000000000000000000', 'cancelled'), ref: 'refs/heads/a-branch-name-that-exceeds-every-narrow-column', finished_at: '2026-11-18T00:00:42Z' },
  ]
  cli(on, () => rows)
  await startSession($, on)
  const wide = await boxLines($, 'terminal', 200)
  expect(wide[1]).toContain('a-repository-name-that-exceeds-every-narrow-column ')
  expect(wide[2]).toContain(' 42s ')
  for (const columns of [100, 64, 48, 40]) {
    for (const surface of ['terminal', 'desktop', 'mobile'] as const) {
      const lines = await boxLines($, surface, columns)
      expect(lines).toHaveLength(3)
      for (const line of lines) expect(line.length).toBeLessThanOrEqual(columns - 4)
      const status = lines[0]!.indexOf('STATUS')
      expect(lines[1]!.indexOf('▶ running')).toBe(status)
      expect(lines[2]!.indexOf('■ cancelled')).toBe(status)
      if (columns < 100) expect(lines[1]!.slice(9, status)).toContain('…')
    }
  }
  const tiny = await boxLines($, 'terminal', 24)
  for (const line of tiny) expect(line.length).toBeLessThanOrEqual(20)
  const ui = await $.ui.mount({ ...band, surface: 'terminal' })
  for (const line of await ui.findAll({ type: 'Text' })) {
    expect(line.props.wrap).toBe('truncate-end')
  }
  await ui.unmount()
})

test('the supported host contract refuses unknown Box and Text props', () => {
  for (const type of ['Box', 'Text'] as const) {
    expect(() => validateTree({ type, props: { unknownProp: true }, children: [] })).toThrow('is not allowed')
  }
  expect(() => validateTree({ type: 'Text', props: { maxLines: 1 }, children: ['run'] })).toThrow('is not allowed')
})

test('the floor host contract refuses newer wrap values and invalid layout values', () => {
  expect(() => validateTree({ type: 'Text', props: { wrap: 'clip' }, children: ['run'] })).toThrow('is not allowed')
  expect(() => validateTree({ type: 'Box', props: { flexDirection: 'grid' }, children: [] })).toThrow('is not allowed')
})

const lintFailure = { failed_step: 'onExit[0].lint', failed_step_name: 'lint', node_id: 'generated-node-419' }

test('selected workflow is visible beside a passing run in the box and list', { options }, async ($, on) => {
  bottom(on)
  mock.clock(on, { now: Date.parse('2026-11-18T00:01:00Z') })
  const selected = { ...run(undefined, 'passed'), finished_at: '2026-11-18T00:00:45Z', selected_file: 'hack/walk-vm.yaml' }
  cli(on, () => [selected])
  await startSession($, on)
  const lines = await boxLines($, 'terminal', 80)
  expect(lines[0]).toMatch(HEADER)
  expect(lines.join('\n')).toContain(selected.selected_file + ' selected')
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text).toContain(selected.selected_file + ' selected')
  expect(() => decodeRun({ ...selected, selected_file: 1 })).toThrow('Invalid workflow file.')
})

test('selected workflow message gives way before box names without changing columns', () => {
  const selected = { ...run(), repo: 'repository-with-a-long-name', ref: 'branch-with-a-long-name', selected_file: 'hack/walk-vm.yaml' }
  const state: Snapshot = { recent: [selected], pins: [], now: Date.parse('2026-11-18T00:01:00Z'), reachable: true, singleProfile: true }
  const plain = { ...state, recent: [{ ...selected, selected_file: undefined }] }
  const ordinary = boxRows(plain, 3, 200)
  const wide = boxRows(state, 3, 200)
  expect(wide[0]).toBe(ordinary[0])
  expect(wide[1]).toContain(selected.selected_file + ' selected')
  expect(boxRows(state, 3, ordinary[1]!.length)).toEqual(boxRows(plain, 3, ordinary[1]!.length))
})

test('/oberth-runs prints the box rows and a reason for failed rows', { options }, async ($, on) => {
  bottom(on)
  mock.clock(on, { now: Date.parse('2026-11-18T00:01:00Z') })
  const failed = { ...run('aaaaaaaa000000000000000000000000', 'failed'), display_reason: 'exited with code 2', failure: lintFailure }
  let rows = [run(), run('bbbbbbbb000000000000000000000000')]
  cli(on, () => rows)
  await startSession($, on)
  const listed = (await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text!.split('\n')
  const box = await boxLines($, 'terminal', commandInput.presentation.columns + 4)
  expect(listed[0]).toBe(box[0] + ' REASON')
  expect(listed.slice(1)).toEqual(box.slice(1))
  rows = [run(), failed]
  const lines = (await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text!.split('\n')
  expect(lines[1]!.endsWith(' 1m')).toBe(true)
  expect(lines[2]).toMatch(/^aaaaaaaa oberth +v0\.24\.0 +✗ failed +6\/11 +1m00s +1m +lint: exited with code 2$/)
  expect(lines[2]!.indexOf('lint')).toBe(lines[0]!.indexOf('REASON'))
  const narrow = await $.command.run({ ...commandInput, presentation: { isFullscreen: false, columns: 40 }, command: 'oberth-runs', args: '' })
  for (const line of narrow.text!.split('\n')) expect(line.length).toBeLessThanOrEqual(40)
  // The reason stays whole; the names give way first.
  rows = [{ ...failed, ref: 'refs/heads/own-v0.30.10', failure: { ...lintFailure, failed_step_name: 'go-tests-pass-with-race' } }]
  const tight = (await $.command.run({ ...commandInput, presentation: { isFullscreen: false, columns: 87 }, command: 'oberth-runs', args: '' })).text!.split('\n')
  expect(tight[1]).toMatch(/^aaaaaaaa obe… +own… +✗ failed .* go-tests-pass-with-race: exited with code 2$/)
  for (const line of tight) expect(line.length).toBeLessThanOrEqual(87)
})

test('/oberth-runs says why a run was cancelled and leaves other reasons out', { options }, async ($, on) => {
  bottom(on)
  mock.clock(on, { now: Date.parse('2026-11-18T00:01:00Z') })
  cli(on, () => [
    { ...run('aaaaaaaa000000000000000000000000', 'cancelled'), display_reason: 'branch deleted' },
    { ...run('bbbbbbbb000000000000000000000000', 'passed'), display_reason: 'passed' },
    { ...run(), display_reason: 'step running' },
  ])
  await startSession($, on)
  const lines = (await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text!.split('\n')
  expect(lines[1]).toMatch(/^aaaaaaaa .*■ cancelled .* branch deleted$/)
  expect(lines[1]!.indexOf('branch deleted')).toBe(lines[0]!.indexOf('REASON'))
  expect(lines[2]).toMatch(/ 1m$/)
  expect(lines[3]).toMatch(/ 1m$/)
})

test('resumed and assisted runs say so on passed rows in the box and /oberth-runs', { options }, async ($, on) => {
  bottom(on)
  mock.clock(on, { now: Date.parse('2026-11-18T00:01:00Z') })
  const finished = { status: 'passed', finished_at: '2026-11-18T00:00:45Z' } as const
  cli(on, () => [
    { ...run('aaaaaaaa000000000000000000000000'), ...finished, display_reason: RESUMED_REASON },
    { ...run('cccccccc000000000000000000000000'), ...finished, display_reason: ASSISTED_REASON },
    { ...run('bbbbbbbb000000000000000000000000'), ...finished, display_reason: 'passed' },
  ])
  await startSession($, on)
  const box = await boxLines($, 'terminal', 120)
  const lines = (await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text!.split('\n')
  for (const rows of [box, lines]) {
    expect(rows.find(line => line.startsWith('aaaaaaaa'))).toMatch(/✓ passed .* resumed; not evidence$/)
    expect(rows.find(line => line.startsWith('cccccccc'))).toMatch(/✓ passed .* assisted; not evidence$/)
    expect(rows.find(line => line.startsWith('bbbbbbbb'))).toMatch(/ \d+[smh]$/)
  }
})

test('a not-evidence reason stays whole at 60 and 80 columns and its pass is amber', { options: { ...options, runs: 5 } }, async ($, on) => {
  bottom(on)
  mock.clock(on, { now: Date.parse('2026-11-18T00:01:00Z') })
  const long = { repo: 'github/oberthci/repository-with-a-long-name', ref: 'refs/heads/w6/fix-resume-long-branch' }
  const finished = { ...long, status: 'passed', finished_at: '2026-11-18T00:00:45Z' } as const
  cli(on, () => [
    { ...run('aaaaaaaa000000000000000000000000'), ...finished, display_reason: RESUMED_REASON },
    { ...run('cccccccc000000000000000000000000'), ...finished, display_reason: ASSISTED_REASON, selected_file: 'hack/walk-vm.yaml' },
    { ...run('bbbbbbbb000000000000000000000000'), ...finished, display_reason: '' },
    { ...run('dddddddd000000000000000000000000'), ...long, status: 'failed', display_reason: RESUMED_REASON, failure: lintFailure },
  ])
  await startSession($, on)
  for (const columns of [60, 80]) {
    const ui = await $.ui.mount({ ...band, props: { ...band.props, bodyColumns: columns }, surface: 'terminal' })
    const drawn = await ui.findAll({ type: 'Text' })
    await ui.unmount()
    const row = (id: string) => drawn.find(line => line.text.startsWith(id))!
    for (const line of drawn) expect(line.text.length).toBeLessThanOrEqual(columns - 4)
    expect(row('aaaaaaaa').text).toEndWith(RESUMED_REASON)
    expect(row('cccccccc').text).toEndWith(ASSISTED_REASON)
    expect(row('dddddddd').text).toContain(RESUMED_REASON)
    expect(row('bbbbbbbb').text).not.toContain('evidence')
    expect([row('aaaaaaaa').props.color, row('cccccccc').props.color, row('bbbbbbbb').props.color, row('dddddddd').props.color]).toEqual(['yellow', 'yellow', 'green', 'red'])
    const listed = (await $.command.run({ ...commandInput, presentation: { isFullscreen: false, columns }, command: 'oberth-runs', args: '' })).text!.split('\n')
    for (const line of listed) expect(line.length).toBeLessThanOrEqual(columns)
    expect(listed.find(line => line.startsWith('aaaaaaaa'))).toContain(RESUMED_REASON)
    expect(listed.find(line => line.startsWith('cccccccc'))).toContain(ASSISTED_REASON)
    expect(listed.find(line => line.startsWith('dddddddd'))).toContain(RESUMED_REASON)
  }
})

test('a failed row names its step and a watched row is marked, cut before the ref', { options }, async ($, on) => {
  bottom(on)
  mock.clock(on, { now: Date.parse('2026-11-18T00:01:00Z') })
  const failed = { ...run('aaaaaaaa000000000000000000000000', 'failed'), ref: 'refs/heads/topic-branch',
    failure: { ...lintFailure, failed_step_name: 'verify-release-artifacts' } }
  cli(on, () => [run(), failed])
  await startSession($, on)
  await $.command.run({ ...commandInput, command: 'oberth-watch', args: run().id })
  const wide = await boxLines($)
  expect(wide[0]).toMatch(HEADER)
  expect(wide[1]).toMatch(/^cad7d9ec .* 1m +●$/)
  expect(wide[2]).toMatch(/^aaaaaaaa oberth +topic-branch +✗ failed +6\/11 +1m00s +1m +verify-release-artifacts$/)
  const tight = await boxLines($, 'terminal', 63)
  expect(tight[2]).toContain(' topic-branch ')
  expect(tight[2]).toMatch(/ ver…$/)
  const narrow = await boxLines($, 'terminal', 62)
  expect(narrow[2]).toMatch(/ 1m$/)
  expect(narrow[1]).toMatch(/ 1m$/)
})

test('a width that is not a usable number of cells lays rows out at 80', { options }, async ($, on) => {
  bottom(on)
  mock.clock(on, { now: Date.parse('2026-11-18T00:01:00Z') })
  const rows = [{ ...run(), ref: 'refs/heads/' + 'x'.repeat(90) }]
  const at80 = table(rows, Date.parse('2026-11-18T00:01:00Z'), 80)
  expect(at80).toContain('▶ running')
  for (const width of [undefined, NaN, 0, 1, -5, Infinity]) {
    expect(table(rows, Date.parse('2026-11-18T00:01:00Z'), width as number)).toBe(at80)
  }
  cli(on, () => rows)
  await startSession($, on)
  const ui = await $.ui.mount({ ...band, props: { ...band.props, bodyColumns: undefined as unknown as number }, surface: 'terminal' })
  const box = (await ui.findAll({ type: 'Text' })).map(line => line.text)
  await ui.unmount()
  expect(box).toEqual(await boxLines($, 'terminal', 80 + 4))
  expect(box[1]).toContain('▶ running')
})

test('durations and ages read as seconds, then two units or one', async () => {
  expect([0, 42000, 190000, 7500000, 100800000].map(ms => span(ms, 2))).toEqual(['0s', '42s', '3m10s', '2h05m', '1d04h'])
  expect([0, 42000, 190000, 7500000, 100800000].map(ms => span(ms, 1))).toEqual(['0s', '42s', '3m', '2h', '1d'])
  const item = { ...run(), finished_at: '2026-11-18T00:00:42Z' }
  expect(duration(item, Date.parse('2026-11-18T01:00:00Z'))).toBe('42s')
  expect(age(item, Date.parse('2026-11-18T01:00:00Z'))).toBe('1h')
  expect(age(item, Date.parse('2026-11-17T23:00:00Z'))).toBe('0s')
})

test('one render time bounds duration by age when native start predates creation', async () => {
  const now = Date.parse('2026-11-18T00:00:20.999Z')
  const item = { ...run(), started_at: '2026-11-17T23:59:59Z' }
  const state: Snapshot = { recent: [item], pins: [], reachable: true, now, singleProfile: true }
  expect(duration(item, now)).toBe('20s')
  expect(age(item, now)).toBe('20s')
  expect(boxRows(state, 3, 120)[1]).toMatch(/ ▶ running +6\/11 +20s +20s$/)
  expect(table([item], now, 120)).toMatch(/ ▶ running +6\/11 +20s +20s$/)
  // The same snapshot crosses the second boundary together in both columns.
  expect(boxRows({ ...state, now: now + 1 }, 3, 120)[1]).toMatch(/ +21s +21s$/)
  expect(duration({ ...item, started_at: '2026-11-18T00:00:10Z' }, now)).toBe('10s')
  expect(duration({ ...item, status: 'queued', started_at: null }, now)).toBe('—')
  expect(duration({ ...item, status: 'passed', finished_at: '2026-11-18T00:00:21Z' }, now)).toBe('20s')
})

for (const clockOffset of [-5000, 5000]) {
  test(`profile clock offset ${clockOffset} bounds duration by age`, async () => {
    const now = Date.parse('2026-11-18T00:00:20.999Z') + clockOffset
    const item = { ...run(), started_at: '2026-11-17T23:59:59Z', clockOffset }
    const state: Snapshot = { recent: [item], pins: [], reachable: true, now, singleProfile: true }
    expect(duration(item, now)).toBe('20s')
    expect(age(item, now)).toBe('20s')
    expect(boxRows(state, 3, 120)[1]).toMatch(/ ▶ running +6\/11 +20s +20s$/)
    expect(table([item], now, 120)).toMatch(/ ▶ running +6\/11 +20s +20s$/)
    expect(duration({ ...item, status: 'passed', finished_at: '2026-11-18T00:00:21Z' }, now)).toBe('20s')
  })
}

test('queued time stays unknown until execution starts', async () => {
  const created = '2026-11-18T00:00:00Z'
  const waiting = { ...run(undefined, 'queued'), created_at: created, started_at: null }
  expect(duration(waiting, Date.parse('2026-11-18T00:01:00Z'))).toBe('—')
  expect(age(waiting, Date.parse('2026-11-18T00:01:00Z'))).toBe('1m')
  const cancelled = { ...waiting, status: 'cancelled' as const, finished_at: '2026-11-18T00:01:00Z' }
  expect(duration(cancelled, Date.parse('2026-11-18T00:01:18Z'))).toBe('—')
  const running = { ...waiting, status: 'running' as const, started_at: '2026-11-18T00:01:00Z' }
  expect(duration(running, Date.parse('2026-11-18T00:01:18Z'))).toBe('18s')
  expect(duration({ ...running, clockOffset: 1000 }, Date.parse('2026-11-18T00:01:19Z'))).toBe('18s')
  expect(duration({ ...running, status: 'queued' }, Date.parse('2026-11-18T00:01:18Z'))).toBe('18s')
})


test('idle reads and refused change waits share one coalesced timer', { options }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on)
  let lists = 0
  let gets = 0
  on('process.run', (_, e) => {
    const detail = e.argv[2] === 'get'
    if (detail) gets++
    else if (e.argv[2] === 'list') lists++
    return { value: { exitCode: 0, stdout: JSON.stringify(detail ? run() : { runs: [] }),
      stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  expect(REFRESH_MS).toBe(15000)
  await startSession($, on)
  await clock.advance(3 * REFRESH_MS)
  expect(lists).toBe(4)
  await $.command.run({ ...commandInput, command: 'oberth-watch', args: run().id })
  await clock.advance(REFRESH_MS)
  expect(lists).toBe(6)
  expect(gets).toBe(3)
  await clock.advance(REFRESH_MS)
  expect(lists).toBe(7)
  expect(gets).toBe(4)
})

test('pin missing from list stays visible, finishes with one toast and stops detail reads', { options }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on, { now: Date.parse('2026-11-18T00:01:00Z') })
  const pinned = run('bbbbbbbb000000000000000000000000')
  let current = pinned
  let gets = 0
  const toasts: string[] = []
  on('ui.toast', (_, e) => { toasts.push(e.text); return { value: undefined } })
  on('process.run', (_, e) => {
    const detail = e.argv[2] === 'get'
    if (detail) gets++
    const data = detail ? current : { runs: [run(), run('cccccccc000000000000000000000000', 'passed'), run('dddddddd000000000000000000000000', 'passed')] }
    return { value: { exitCode: 0, stdout: JSON.stringify(data), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  await startSession($, on)
  expect((await $.command.run({ ...commandInput, command: 'oberth-watch', args: 'bbbbbbbb' })).text).toContain('Watching')
  await clock.advance(60000)
  expect(gets).toBe(6)
  expect((await boxLines($))[1]).toMatch(/^bbbbbbbb oberth +v0\.24\.0 +▶ running +6\/11 /)
  current = { ...pinned, status: 'failed', failed: 1, failure: { failed_step: 'onExit[0].lint', failed_step_name: 'lint', node_id: 'generated-node-419' } } as Run
  await clock.advance(15000)
  expect(toasts).toEqual([`Oberth ${pinned.id}: failed; step lint`])
  await clock.advance(60000)
  expect(gets).toBe(7)
  expect(toasts.length).toBe(1)
  const finished = await $.command.run({ ...commandInput, command: 'oberth-watch', args: pinned.id })
  expect(finished.text).toBe(`${pinned.id}: failed; step lint; already finished.`)
  expect(toasts.length).toBe(1)
})

test('unreachable and truncated responses hide stale rows and name the unreadable profile', { options }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on)
  let exitCode = 0
  let truncated = false
  let truncatedDiagnostic = false
  let stderr = ''
  on('process.run', () => ({ value: { exitCode, stdout: JSON.stringify({ runs: [run()] }), stderr, isStdoutTruncated: truncated, isStderrTruncated: truncatedDiagnostic } }))
  await startSession($, on)
  await text($, ROW)
  exitCode = 1
  stderr = 'The run selector is ambiguous.\nUse the complete run ID from the run table.'
  await clock.advance(15000)
  const ui = await $.ui.mount({ ...band, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /cad7d9ec/ })).toBeUndefined()
  await ui.unmount()
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text).toBe(stderr)
  truncatedDiagnostic = true
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text).toBe('Run output exceeded the host output limit.')
  truncatedDiagnostic = false
  exitCode = 0
  truncated = true
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text).toBe('Run output exceeded the host output limit.')
})

test('table command shows the status word and no native names', { options }, async ($, on) => {
  bottom(on)
  mock.clock(on)
  const item = { ...run(undefined, 'failed'), display_reason: 'step failed',
    failure: { failed_step: 'onExit[0].verify-release-artifacts', failed_step_name: 'verify-release-artifacts', node_id: 'generated-node-419' },
    steps: [{ name: 'verify-release-artifacts', pod_name: 'generated-pod-928' }] }
  const calls: string[][] = []
  on('process.run', (_, e) => {
    calls.push([...e.argv])
    return { value: { exitCode: 0, stdout: JSON.stringify({ runs: [item] }), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  await startSession($, on)
  const command = await $.command.run({ ...commandInput, command: 'oberth-runs', args: '--limit=7' })
  expect(command.text).toBe((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '--limit 7' })).text)
  expect(command.text!.split('\n')[1]).toMatch(/^cad7d9ec oberth +v0\.24\.0 +✗ failed +6\/11 /)
  expect(command.text).not.toContain('onExit[0]')
  expect(command.text).not.toContain('generated-node-419')
  expect(command.text).not.toContain('generated-pod-928')
  expect(calls[1]![4]).toBe('7')
  expect(calls[2]).toEqual(calls[1])
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '--limit=0' })).text).toContain('1 to 50')
})

test('short IDs disambiguate and non-hex IDs stay whole; unknown plans remain inexact', async () => {
  const first = run()
  const second = run('cad7d9ec100000000000000000000000')
  expect(shortID(first.id, [first, second])).toBe('cad7d9ec0')
  expect(shortID('argo:parent:child', [])).toBe('argo:parent:child')
  expect(progress({ ...first, steps_planned_exact: false })).toBe('6/≥11')
  expect(progress({ ...first, failed: 2, skipped: 1 })).toBe('9/11')
  expect(() => decodeRun({ ...first, step_count: undefined })).toThrow()
  expect(visible({ recent: [first], pins: [], now: 0, reachable: false, singleProfile: false }, 3)).toEqual([])
  expect(table([{ ...first, ref: 'ref\u001b\u200b' }], 0, 100)).not.toContain('\u001b')
  expect(table([{ ...first, ref: 'ref\u001b\u200b' }], 0, 100)).not.toContain('\u200b')
})

test('resolved checkout executable refuses without spawning', { options }, async ($, on) => {
  bottom(on, '/checkout/bin/oberth')
  on('process.run', () => { throw new Error('must not spawn') })
  mock.clock(on)
  await startSession($, on)
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text).toContain('Checkout executable refused')
})

test('a configured installation beneath a non-checkout project root is allowed', { options: { ...options, executable: '/checkout/tools/oberth' } }, async ($, on) => {
  bottom(on, undefined, false)
  mock.clock(on)
  const calls: string[][] = []
  on('process.run', (_, e) => {
    calls.push([...e.argv])
    return { value: { exitCode: 0, stdout: JSON.stringify({ runs: [] }), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  await startSession($, on)
  expect(calls[0]![0]).toBe('/checkout/tools/oberth')
})

test('session end cancels the sole refresh timer and clears the box', { options }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on)
  let calls = 0
  on('process.run', (_, e) => {
    if (e.argv[2] === 'list') calls++
    return { value: { exitCode: 0, stdout: JSON.stringify({ runs: [run()] }), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  await startSession($, on)
  await clock.advance(15000)
  expect(calls).toBe(2)
  await $.session.end({ sessionId: 'test-session', resume: { id: 'test-session' }, reason: 'clear' })
  await clock.advance(15000)
  expect(calls).toBe(3)
  await $.session.end({ sessionId: 'test-session', resume: { id: 'test-session' }, reason: 'prompt_input_exit' })
  await clock.advance(60000)
  expect(calls).toBe(3)
  const ui = await $.ui.mount({ ...band, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /cad7d9ec/ })).toBeUndefined()
  await ui.unmount()
})

test('listed pins share the list refresh and a fourth active pin is refused', { options }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on)
  const rows = [run(), run('aaaaaaaa000000000000000000000000'), run('bbbbbbbb000000000000000000000000')]
  const extra = run('cccccccc000000000000000000000000')
  let gets = 0
  on('process.run', (_, e) => {
    const detail = e.argv[2] === 'get'
    if (detail) gets++
    const data = detail ? [...rows, extra].find(run => run.id === e.argv[3]) : { runs: rows }
    return { value: { exitCode: 0, stdout: JSON.stringify(data), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  await startSession($, on)
  for (const run of rows) await $.command.run({ ...commandInput, command: 'oberth-watch', args: run.id })
  await clock.advance(60000)
  expect(gets).toBe(3)
  expect((await $.command.run({ ...commandInput, command: 'oberth-watch', args: extra.id })).text).toContain('Three runs are already pinned')
  expect((await $.command.run({ ...commandInput, command: 'oberth-watch', args: '--json' })).text).toContain('Use /oberth-watch')
})

test('a competing state write cannot duplicate a completion toast', { options }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on)
  let item = run()
  let stored: { value: { shape: string; value: Snapshot }; version: number } = {
    value: { shape: 'runs-1', value: { recent: [], pins: [], reachable: false, now: 0, singleProfile: false } }, version: 0,
  }
  let conflict = true
  const toasts: string[] = []
  on('state.get', { plugin: 'oberth', key: 'snapshot' }, () => ({ value: stored }))
  on('state.set', { plugin: 'oberth', key: 'snapshot' }, (_, e) => {
    if (conflict && e.value.value.pins.some(pin => pin.doneAt !== null)) {
      conflict = false
      stored.version++
      return { value: { isSet: false, version: stored.version } }
    }
    stored = { value: e.value, version: stored.version + 1 }
    return { value: { isSet: true, version: stored.version } }
  })
  on('ui.toast', (_, e) => { toasts.push(e.text); return { value: undefined } })
  on('process.run', (_, e) => ({ value: {
    exitCode: 0, stdout: JSON.stringify(e.argv[2] === 'get' ? item : { runs: [item] }),
    stderr: '', isStdoutTruncated: false, isStderrTruncated: false,
  } }))
  await startSession($, on)
  await $.command.run({ ...commandInput, command: 'oberth-watch', args: item.id })
  item = { ...item, status: 'passed' }
  await clock.advance(15000)
  expect(conflict).toBe(false)
  expect(toasts).toEqual([`Oberth ${item.id}: passed`])
})

const six = [1, 2, 3, 4, 5, 6].map(n => run(String(n).repeat(8) + '0'.repeat(24)))

for (const pinFirst of [false, true]) {
  test(`three live rows use one wait per round${pinFirst ? ' and prefer a watched pin' : ''}`, { options }, async ($, on) => {
    bottom(on)
    const clock = mock.clock(on, { now: Date.parse('2026-11-18T00:01:00Z') })
    let rows = six.slice(0, 3)
    const pinned = run('dddddddd000000000000000000000000')
    let lists = 0
    const waits: string[] = []
    let releaseFirst!: () => void
    let releaseRest!: () => void
    let nextRound!: () => void
    const first = new Promise<void>(resolve => { releaseFirst = resolve })
    const rest = new Promise<void>(resolve => { releaseRest = resolve })
    const advanced = new Promise<void>(resolve => { nextRound = resolve })
    on('process.run', async (_, e) => {
      let value: unknown
      if (e.argv[2] === 'wait') {
        const id = e.argv[3]!
        waits.push(id)
        if (waits.length === 2) nextRound()
        await (id === six[0]!.id ? first : rest)
        value = { run: id === pinned.id ? pinned : rows.find(row => row.id === id), timed_out: false }
      } else if (e.argv[2] === 'get') value = pinned
      else { lists++; value = { runs: rows } }
      return { value: { exitCode: 0, stdout: JSON.stringify(value), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
    })
    try {
      await startSession($, on)
      await boxLines($)
      expect(lists).toBe(1)
      expect(waits).toEqual([six[0]!.id])
      if (pinFirst) await $.command.run({ ...commandInput, command: 'oberth-watch', args: pinned.id })
      rows = rows.map((row, index) => index ? row : { ...row, status: 'passed', finished_at: '2026-11-18T00:01:00.200Z' })
      await clock.advance(200)
      releaseFirst()
      await advanced
      expect(lists).toBe(2)
      expect(waits).toEqual([six[0]!.id, pinFirst ? pinned.id : six[1]!.id])
      expect((await boxLines($)).some(line => line.startsWith('11111111') && line.includes('✓ passed'))).toBe(true)
    } finally {
      await $.session.end({ sessionId: 'test-session', resume: { id: 'test-session' }, reason: 'prompt_input_exit' })
      releaseFirst()
      releaseRest()
    }
  })
}

test('a watch added during a pending wait uses its server clock before any refresh', { options }, async ($, on) => {
  bottom(on)
  const server = Date.parse('2026-11-18T00:01:00Z')
  mock.clock(on, { now: server + 11 * 60000 })
  const listed = run()
  const watched = run('dddddddd000000000000000000000000')
  let lists = 0
  let release!: () => void
  let armed!: () => void
  const held = new Promise<void>(resolve => { release = resolve })
  const waiting = new Promise<void>(resolve => { armed = resolve })
  on('process.run', async (_, e) => {
    let value: unknown
    if (e.argv[2] === 'wait') { armed(); await held; value = { run: listed, timed_out: true } }
    else if (e.argv[2] === 'get') value = watched
    else { lists++; value = { runs: [listed], server_time: new Date(server).toISOString() } }
    return { value: { exitCode: 0, stdout: JSON.stringify(value), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  try {
    await startSession($, on)
    await waiting
    await $.command.run({ ...commandInput, command: 'oberth-watch', args: watched.id })
    expect(lists).toBe(1)
    expect((await boxLines($))[1]).toMatch(/^dddddddd .*▶ running +6\/11 +1m00s +1m +●$/)
    expect(lists).toBe(1)
  } finally {
    await $.session.end({ sessionId: 'test-session', resume: { id: 'test-session' }, reason: 'prompt_input_exit' })
    release()
  }
})

test('a live run changes in the box within one second through a bounded change wait', { options }, async ($, on) => {
  bottom(on)
  let server = Date.parse('2026-11-18T00:01:00Z')
  const clock = mock.clock(on, { now: server })
  let current = run()
  const waits: string[][] = []
  let release!: () => void
  const changed = new Promise<void>(resolve => { release = resolve })
  on('process.run', async (_, e) => {
    if (e.argv[2] === 'wait') {
      waits.push([...e.argv])
      expect(e.init?.env?.OBERTH_PROFILE).toBe(options.profile)
      await changed
      return { value: { exitCode: 0, stdout: JSON.stringify({ run: current, timed_out: current.status === 'running' }),
        stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
    }
    return { value: { exitCode: 0, stdout: JSON.stringify({ runs: [current], server_time: new Date(server).toISOString() }),
      stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  await startSession($, on)
  expect((await boxLines($))[1]).toContain('▶ running')
  current = { ...current, status: 'passed', finished_at: '2026-11-18T00:01:01Z' }
  server += 1000
  await clock.advance(1000)
  const refreshed = waits.length ? nextSnapshot(on) : undefined
  release()
  if (refreshed) await refreshed
  expect((await boxLines($))[1]).toContain('✓ passed')
  expect(waits).toEqual([['/verified/oberth', 'run', 'wait', current.id, '--until', 'any', '--timeout-s', '1', '--json']])
})

for (const phase of [0, 250, 500, 750, 999]) {
  test(`an unwaited live row refreshes within one second after a change at ${phase}ms`, { options }, async ($, on) => {
    bottom(on)
    const start = Date.parse('2026-11-18T00:01:00Z')
    const clock = mock.clock(on, { now: start })
    let rows = six.slice(0, 3)
    let lists = 0
    let release!: () => void
    let armed!: () => void
    const held = new Promise<void>(resolve => { release = resolve })
    const waiting = new Promise<void>(resolve => { armed = resolve })
    const waits: string[][] = []
    let observedAt = start
    on('state.set', { plugin: 'oberth', key: 'snapshot' }, (_, e, next) => {
      observedAt = e.value.value.now
      return next(e)
    })
    on('process.run', async (_, e) => {
      let value: unknown
      if (e.argv[2] === 'wait') {
        waits.push([...e.argv])
        armed()
        await held
        value = { run: rows[0], timed_out: true }
      } else {
        lists++
        value = { runs: rows }
      }
      return { value: { exitCode: 0, stdout: JSON.stringify(value), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
    })
    try {
      await startSession($, on)
      await waiting
      await clock.advance(phase)
      const changedAt = start + phase
      rows = rows.map(row => ({ ...row, status: 'passed' as const, finished_at: new Date(start + phase).toISOString() }))
      await clock.advance(1000 - phase)
      const refreshed = nextSnapshot(on)
      release()
      await refreshed
      expect(observedAt - changedAt).toBe(1000 - phase)
      expect((await boxLines($)).slice(1).every(line => line.includes('✓ passed'))).toBe(true)
      expect((await boxLines($)).slice(1)).toHaveLength(3)
      expect(lists).toBe(2)
      expect(waits).toEqual([['/verified/oberth', 'run', 'wait', six[0]!.id, '--until', 'any', '--timeout-s', '1', '--json']])
    } finally {
      await $.session.end({ sessionId: 'test-session', resume: { id: 'test-session' }, reason: 'prompt_input_exit' })
      release()
    }
  })
}

for (const rows of [[], [{ ...run(), status: 'passed' as const, finished_at: '2026-11-18T00:01:00Z' }]]) {
  test(`idle polling stays at fifteen seconds with ${rows.length ? 'a finished run' : 'no runs'}`, { options }, async ($, on) => {
    bottom(on)
    const clock = mock.clock(on, { now: Date.parse('2026-11-18T00:01:00Z') })
    const calls = cli(on, () => rows)
    await startSession($, on)
    await clock.advance(14999)
    expect(calls).toHaveLength(1)
    await clock.advance(1)
    expect(calls).toHaveLength(2)
    expect(calls.every(args => args[2] === 'list')).toBe(true)
  })
}

const multiple = { executable: options.executable, profile: ' first, second, first ', runs: 2 }

test('a single unreadable profile uses one fitting prompt line after a successful read', { options }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on)
  let failed = false
  on('process.run', () => ({ value: { exitCode: failed ? 1 : 0, stdout: JSON.stringify({ runs: [] }),
    stderr: failed ? 'Identity expired. Run oberth join again.' : '', isStdoutTruncated: false, isStderrTruncated: false } }))
  await startSession($, on)
  failed = true
  await clock.advance(REFRESH_MS)
  expect(PROFILE_REASON.length).toBeLessThanOrEqual(40)
  for (const surface of ['terminal', 'desktop', 'mobile'] as const) {
    for (const columns of [100, 24, 20]) {
      const ui = await $.ui.mount({ ...band, props: { ...band.props, bodyColumns: columns }, surface })
      const lines = await ui.findAll({ type: 'Text' })
      expect(lines).toHaveLength(1)
      expect(lines[0]?.text).toBe(PROFILE_REASON)
      expect(lines[0]?.props.wrap).toBe('truncate-end')
      expect(lines[0]!.text.length).toBeLessThanOrEqual(columns)
      expect((await ui.findAll({ type: 'Box' })).some(box => box.props.borderStyle === 'round')).toBe(false)
      await ui.unmount()
    }
    const narrow = await $.ui.mount({ ...band, props: { ...band.props, bodyColumns: 19 }, surface })
    expect(await narrow.findAll({ type: 'Text' })).toHaveLength(0)
    await narrow.unmount()
  }
})

for (const selection of [options, multiple]) {
  test(`/oberth-runs keeps the CLI refusal with ${selection === options ? 'one profile' : 'a healthy empty peer'}`, { options: selection }, async ($, on) => {
    bottom(on)
    mock.clock(on)
    let reason = 'Identity expired. Run oberth join again.'
    on('process.run', (_, e) => {
      const failed = selection === options || e.init?.env?.OBERTH_PROFILE === 'second'
      return { value: { exitCode: failed ? 1 : 0, stdout: JSON.stringify({ runs: [] }), stderr: failed ? reason : '',
        isStdoutTruncated: false, isStderrTruncated: false } }
    })
    await startSession($, on)
    const reply = (await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text!
    expect(reply).toBe(selection === options ? reason : `Profile "second": ${reason}`)
    expect(reply).not.toContain('No runs.')
    if (selection === multiple) expect((await boxLines($)).join('\n')).toContain('Profile "second": Could not read runs.')
    reason = 'x'.repeat(80)
    const full = (await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text!
    expect(full).toBe(selection === options ? reason : `Profile "second": ${reason}`)
    reason = 'x'.repeat(81)
    const bounded = (await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text!
    const fallback = `${PROFILE_REASON} ${PROFILE_REMEDY}`
    expect(fallback.length).toBeLessThanOrEqual(80)
    expect(bounded).toBe(selection === options ? fallback : `Profile "second": ${fallback}`)
  })
}

test('a live unlisted watch keeps its profile clock and other profile warnings during change waits', { options: multiple }, async ($, on) => {
  bottom(on)
  let server = Date.parse('2026-11-18T00:01:00Z')
  const clock = mock.clock(on, { now: server + 11 * 60000 })
  const notices = toasts(on)
  let current = run('dddddddd000000000000000000000000')
  let release!: () => void
  let waiting!: () => void
  const changed = new Promise<void>(resolve => { release = resolve })
  const started = new Promise<void>(resolve => { waiting = resolve })
  on('process.run', async (_, e) => {
    if (e.init?.env?.OBERTH_PROFILE === 'first') throw new Error('Unavailable profile.')
    let value: unknown
    if (e.argv[2] === 'wait') {
      expect(e.init?.env?.OBERTH_PROFILE).toBe('second')
      waiting()
      await changed
      value = { run: current, timed_out: false }
    } else value = e.argv[2] === 'get' ? current : { runs: [], server_time: new Date(server).toISOString() }
    return { value: { exitCode: 0, stdout: JSON.stringify(value), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  await startSession($, on)
  await $.command.run({ ...commandInput, command: 'oberth-watch', args: current.id })
  await started
  expect((await boxLines($))[1]).toMatch(/^dddddddd .*▶ running +6\/11 +1m00s +1m +●$/)
  current = { ...current, status: 'passed', finished_at: '2026-11-18T00:01:01Z' }
  server += 1000
  await clock.advance(1000)
  const refreshed = nextSnapshot(on)
  release()
  await refreshed
  const lines = await boxLines($)
  expect(lines[1]).toMatch(/^dddddddd .*✓ passed +6\/11 +1m01s +1m +●$/)
  expect(lines.join('\n')).toContain('Profile "first": Could not read runs.')
  expect(notices).toEqual([`Oberth ${current.id}: passed`])
})

test('a new session retires pending live waits and watch reads without delaying its idle view', { options }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on)
  const old = run()
  let rows = [old]
  let lists = 0
  let releaseWait!: () => void
  let releaseGet!: () => void
  let waitStarted!: () => void
  let getStarted!: () => void
  const wait = new Promise<void>(resolve => { releaseWait = resolve })
  const get = new Promise<void>(resolve => { releaseGet = resolve })
  const waiting = new Promise<void>(resolve => { waitStarted = resolve })
  const getting = new Promise<void>(resolve => { getStarted = resolve })
  on('process.run', async (_, e) => {
    let value: unknown
    if (e.argv[2] === 'wait') { waitStarted(); await wait; value = { run: old, timed_out: true } }
    else if (e.argv[2] === 'get') { getStarted(); await get; value = old }
    else { lists++; value = { runs: rows } }
    return { value: { exitCode: 0, stdout: JSON.stringify(value), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  await startSession($, on)
  await waiting
  const watched = $.command.run({ ...commandInput, command: 'oberth-watch', args: old.id })
  await getting
  await $.session.end({ sessionId: 'test-session', resume: { id: 'test-session' }, reason: 'prompt_input_exit' })
  rows = []
  await startSession($, on)
  await noBox($)
  releaseWait()
  releaseGet()
  await watched
  await clock.advance(0)
  await noBox($)
  expect(lists).toBe(2)
  await clock.advance(14999)
  expect(lists).toBe(2)
  await clock.advance(1)
  expect(lists).toBe(3)
})

for (const selection of [options, multiple]) {
  for (const skew of [11 * 60000, -30 * 60000]) {
    test(`an unlisted watch uses its profile clock with ${selection === options ? 'one' : 'several'} profiles and skew ${skew}`,
      { options: { ...selection, runs: 1 } }, async ($, on) => {
        bottom(on)
        let server = Date.parse('2026-11-18T00:01:00Z')
        const clock = mock.clock(on, { now: server + skew })
        const notices = toasts(on)
        let watched = run('dddddddd000000000000000000000000')
        const source = selection === options ? options.profile : 'second'
        on('process.run', (_, e) => {
          const selected = e.init?.env?.OBERTH_PROFILE === source
          const value = e.argv[2] === 'get' ? (selected ? watched : undefined) : {
            runs: [], server_time: new Date(server + (selected ? 0 : 2 * 3600000)).toISOString(),
          }
          return { value: { exitCode: value ? 0 : 1, stdout: value ? JSON.stringify(value) : '',
            stderr: value ? '' : 'Run not found.', isStdoutTruncated: false, isStderrTruncated: false } }
        })
        const advance = async () => { server += REFRESH_MS; await clock.advance(REFRESH_MS) }
        await startSession($, on)
        await $.command.run({ ...commandInput, command: 'oberth-watch', args: watched.id })
        expect((await boxLines($))[1]).toMatch(/^dddddddd .*▶ running +6\/11 +1m00s +1m +●$/)
        await advance()
        expect((await boxLines($))[1]).toMatch(/1m15s +1m +●$/)
        watched = { ...watched, status: 'passed', finished_at: new Date(server + REFRESH_MS).toISOString() }
        await advance()
        expect((await boxLines($))[1]).toMatch(/^dddddddd .*✓ passed +6\/11 +1m30s +1m +●$/)
        expect(notices).toEqual([`Oberth ${watched.id}: passed`])
      })
  }
}

test('configured profiles merge upstreams newest first with one global row limit and each server clock', { options: multiple }, async ($, on) => {
  bottom(on)
  mock.clock(on, { now: Date.parse('2026-11-18T00:10:00Z') })
  const calls: string[] = []
  on('process.run', (_, e) => {
    if (e.argv[2] === 'wait') throw new Error('Change wait unavailable in this list fixture.')
    const name = String(e.init?.env?.OBERTH_PROFILE)
    calls.push(name)
    expect(e.init?.timeoutMs).toBe(3500)
    expect(e.argv).toEqual(['/verified/oberth', 'run', 'list', '--limit', '2', '--json'])
    const data = name === 'first' ? {
      runs: [{ ...six[0]!, repo: 'one/team/widget', created_at: '2026-11-18T00:08:00Z', started_at: '2026-11-18T00:08:00Z' }, six[2]!],
      server_time: '2026-11-18T00:10:00Z',
    } : {
      runs: [{ ...six[1]!, repo: 'two/team/gadget', created_at: '2026-11-18T01:09:00Z', started_at: '2026-11-18T01:09:00Z' }],
      server_time: '2026-11-18T01:10:00Z',
    }
    return { value: { exitCode: 0, stdout: JSON.stringify(data), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  await startSession($, on)
  expect(calls).toEqual(['first', 'second'])
  for (const surface of ['terminal', 'desktop', 'mobile'] as const) {
    const lines = await boxLines($, surface)
    expect(lines).toHaveLength(3)
    expect(lines[1]).toMatch(/^22222222 gadget .*1m00s +1m$/)
    expect(lines[2]).toMatch(/^11111111 widget .*2m00s +2m$/)
  }
  const result = await $.command.run({ ...commandInput, command: 'oberth-runs', args: '--limit=2' })
  expect(result.text!.split('\n').slice(1).map(line => line.slice(0, 8))).toEqual(['22222222', '11111111'])
  expect(calls).toEqual(['first', 'second', 'first', 'second'])
})

test('a watch finds the other profile and polls only its source after it leaves the merged list', { options: multiple }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on, { now: Date.parse('2026-11-18T00:01:00Z') })
  const notices = toasts(on)
  let watched = { ...six[1]!, repo: 'two/team/gadget' }
  let listed = true
  let unavailable = false
  const details: string[] = []
  on('process.run', (_, e) => {
    const name = String(e.init?.env?.OBERTH_PROFILE)
    if (e.argv[2] === 'get') details.push(name)
    const found = name === 'second' && !unavailable
    const data = e.argv[2] === 'get' ? (found ? watched : undefined) : { runs: found && listed ? [watched] : [six[0]!] }
    return { value: { exitCode: data ? 0 : 1, stdout: data ? JSON.stringify(data) : '', stderr: data ? '' : 'Run not found.', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  await startSession($, on)
  expect((await $.command.run({ ...commandInput, command: 'oberth-watch', args: watched.id })).text).toContain('Watching')
  expect(details).toEqual(['first', 'second'])
  listed = false
  unavailable = true
  await clock.advance(REFRESH_MS)
  expect((await boxLines($)).some(line => line.startsWith('22222222'))).toBe(false)
  unavailable = false
  watched = { ...watched, status: 'passed', finished_at: '2026-11-18T00:01:10Z' }
  await clock.advance(REFRESH_MS)
  expect(details).toEqual(['first', 'second', 'second', 'second', 'second'])
  expect((await boxLines($))[1]).toMatch(/^22222222 gadget .*✓ passed .*●$/)
  expect(notices).toEqual([`Oberth ${watched.id}: passed`])
  await clock.advance(REFRESH_MS)
  expect(details).toHaveLength(5)
})

test('overlapping profiles deduplicate runs and an unavailable profile leaves healthy rows visible', { options: multiple }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on, { now: Date.parse('2026-11-18T00:01:00Z') })
  let unavailable = false
  on('process.run', (_, e) => {
    if (unavailable && e.init?.env?.OBERTH_PROFILE === 'second') throw new Error('unreachable')
    return { value: { exitCode: 0, stdout: JSON.stringify({ runs: [run()] }), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  await startSession($, on)
  expect(await boxLines($)).toHaveLength(2)
  unavailable = true
  await clock.advance(REFRESH_MS)
  expect(await boxLines($)).toHaveLength(3)
})

test('unreadable profiles are named in the box and table until recovery', { options: multiple }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on)
  let firstFails = false
  let secondFails = true
  on('process.run', (_, e) => {
    const failed = e.init?.env?.OBERTH_PROFILE === 'first' ? firstFails : secondFails
    return { value: { exitCode: failed ? 1 : 0, stdout: JSON.stringify({ runs: [run()] }),
      stderr: failed ? 'The profile could not connect.' : '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  const message = 'Profile "second": Could not read runs. Check the joined profile and server connection.'
  expect(PROFILE_REASON.length).toBeLessThanOrEqual(40)
  expect(PROFILE_REMEDY.length).toBeLessThanOrEqual(120)
  await startSession($, on)
  for (const surface of ['terminal', 'desktop', 'mobile'] as const) {
    const ui = await $.ui.mount({ ...band, props: { ...band.props, bodyColumns: 24 }, surface })
    const failure = await ui.find({ type: 'Text', text: message })
    expect(failure?.props.wrap).toBe('wrap')
    expect((await ui.findAll({ type: 'Box' })).some(box => box.props.width === 24)).toBe(true)
    await ui.unmount()
  }
  let output = (await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text!
  expect(output).toContain('cad7d9ec')
  expect(output).toContain('Profile "second": The profile could not connect.')
  firstFails = true
  await clock.advance(REFRESH_MS)
  output = (await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text!
  expect(output).toContain('Profile "first": The profile could not connect.')
  expect(output).toContain('Profile "second": The profile could not connect.')
  expect(output).not.toContain('cad7d9ec')
  firstFails = secondFails = false
  await clock.advance(REFRESH_MS)
  expect((await boxLines($)).some(line => line.startsWith('Profile '))).toBe(false)
})

test('a watch detail cannot clear a failed profile list or invent its clock', { options: { ...options, runs: 1 } }, async ($, on) => {
  bottom(on)
  let server = Date.parse('2026-11-18T00:01:00Z')
  const clock = mock.clock(on, { now: server + 11 * 60000 })
  const watched = run('dddddddd000000000000000000000000')
  let listFails = true
  on('process.run', (_, e) => {
    const detail = e.argv[2] === 'get'
    return { value: { exitCode: !detail && listFails ? 1 : 0,
      stdout: JSON.stringify(detail ? watched : { runs: [], server_time: new Date(server).toISOString() }),
      stderr: listFails ? 'List unavailable.' : '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  const advance = async () => { server += REFRESH_MS; await clock.advance(REFRESH_MS) }
  await startSession($, on)
  await $.command.run({ ...commandInput, command: 'oberth-watch', args: watched.id })
  await advance()
  const unavailable = await boxLines($)
  expect(unavailable).toEqual([PROFILE_REASON])
  expect(unavailable.some(line => line.startsWith('dddddddd'))).toBe(false)
  listFails = false
  await advance()
  expect((await boxLines($))[1]).toMatch(/^dddddddd .*▶ running +6\/11 +1m30s +1m +●$/)
})

test('a blank profile option keeps the environment selection', { options: { ...options, profile: ' ' } }, async ($, on) => {
  bottom(on, undefined, true, options.profile)
  mock.clock(on)
  const calls = cli(on, () => [])
  await startSession($, on)
  expect(calls).toHaveLength(1)
})

test('a mistyped profile list explains setup again after an earlier notice', { options: { ...options, profile: 'a,,b' } }, async ($, on) => {
  const store = bottom(on)
  store.set('profile-notice', true)
  const notices = toasts(on)
  await startSession($, on)
  await startSession($, on)
  expect(notices).toHaveLength(1)
  expect(notices[0]).toContain('comma-separated joined profile names')
  expect(store.get('profile-notice')).toBe('a,,b')
})

for (const profile of ['first,,second', Array(33).fill(0).map((_, n) => `p${n}`).join(',')]) {
  test(`invalid profile list ${profile} explains setup once without reading runs`, { options: { ...options, profile } }, async ($, on) => {
    bottom(on)
    const clock = mock.clock(on)
    const notices = toasts(on)
    const calls = cli(on, () => [])
    await startSession($, on)
    await startSession($, on)
    await clock.advance(REFRESH_MS)
    expect(calls).toHaveLength(0)
    expect(notices).toHaveLength(1)
    expect(notices[0]).toContain('comma-separated joined profile names')
    expect(notices[0]!.length).toBeLessThanOrEqual(80)
    await noBox($)
  })
}

test('a prefix matching different runs across profiles refuses a watch', { options: multiple }, async ($, on) => {
  bottom(on)
  mock.clock(on)
  const ids = ['cad7d9ec000000000000000000000000', 'cad7d9ec100000000000000000000000']
  on('process.run', (_, e) => ({ value: { exitCode: 0,
    stdout: JSON.stringify(e.argv[2] === 'get' ? run(ids[e.init?.env?.OBERTH_PROFILE === 'first' ? 0 : 1]) : { runs: [] }),
    stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }))
  await startSession($, on)
  const answer = await $.command.run({ ...commandInput, command: 'oberth-watch', args: 'cad7d9ec' })
  expect(answer.text).toBe('Run prefix matches several profiles. Use the full run ID.')
  expect(answer.text!.length).toBeLessThanOrEqual(80)
  await noBox($)
})

function cli(on: On, rows: () => Run[]) {
  const calls: string[][] = []
  on('process.run', (_, e) => {
    calls.push([...e.argv])
    const data = e.argv[2] === 'get' ? rows().find(run => run.id === e.argv[3]) : { runs: rows() }
    return { value: { exitCode: 0, stdout: JSON.stringify(data), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  return calls
}

function toasts(on: On) {
  const texts: string[] = []
  on('ui.toast', (_, e) => { texts.push(e.text); return { value: undefined } })
  return texts
}

function refreshes(on: On) {
  const count = { value: 0 }
  on('state.set', { plugin: 'oberth', key: 'snapshot' }, (_, e, next) => { count.value++; return next(e) })
  return count
}

async function noBox($: Engine) {
  for (const surface of ['terminal', 'desktop', 'mobile'] as const) {
    const ui = await $.ui.mount({ ...band, surface })
    expect(await ui.findAll({ type: 'Text' })).toHaveLength(0)
    await ui.unmount()
  }
}

for (const [name, runs, count] of [['unset', undefined, 3], ['empty', '', 3], ['1', 1, 1], ['5', 5, 5]] as const) {
  test(`runs ${name} lists and shows ${count} runs`, { options: runs === undefined ? options : { ...options, runs } }, async ($, on) => {
    bottom(on)
    mock.clock(on)
    const calls = cli(on, () => six)
    await startSession($, on)
    expect(calls).toEqual([['/verified/oberth', 'run', 'list', '--limit', String(count), '--json']])
    for (const surface of ['terminal', 'desktop', 'mobile'] as const) {
      const ui = await $.ui.mount({ ...band, surface })
      const lines = await ui.findAll({ type: 'Text' })
      expect(lines.map(line => line.text.split(' ')[0]).slice(1)).toEqual(six.slice(0, count).map(run => run.id.slice(0, 8)))
      await ui.unmount()
    }
  })
}

test('runs 0 draws no box and never refreshes it', { options: { ...options, runs: 0 } }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on)
  const stored = { value: { shape: 'runs-1', value: { recent: [run()], pins: [], reachable: true, now: 0 } }, version: 1 }
  on('state.get', { plugin: 'oberth', key: 'snapshot' }, () => ({ value: stored }))
  const written = refreshes(on)
  const calls = cli(on, () => six)
  const notices = toasts(on)
  await $.session.start(start)
  await clock.advance(180000)
  expect(calls).toEqual([])
  expect(written.value).toBe(0)
  expect(notices).toEqual([])
  await noBox($)
})

for (const runs of [11, -1, 2.5]) {
  test(`runs ${runs} hides the box with one notice and never refreshes it`, { options: { ...options, runs } }, async ($, on) => {
    bottom(on)
    const clock = mock.clock(on)
    const written = refreshes(on)
    const calls = cli(on, () => six)
    const notices = toasts(on)
    await $.session.start(start)
    await $.session.start(start)
    await clock.advance(180000)
    expect(notices).toEqual([`${SHOWN_REASON} ${SHOWN_REMEDY}`])
    expect(SHOWN_REASON.length).toBeLessThanOrEqual(40)
    expect(SHOWN_REMEDY.length).toBeLessThanOrEqual(120)
    expect(calls).toEqual([])
    expect(written.value).toBe(0)
    await noBox($)
    expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '--limit=6' })).text).toContain(six[5]!.id.slice(0, 8))
  })
}

test('runs 0 keeps the table and watches with completion toasts', { options: { ...options, runs: 0 } }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on)
  let rows = [run(), run('aaaaaaaa000000000000000000000000')]
  const calls = cli(on, () => rows)
  const notices = toasts(on)
  await $.session.start(start)
  const listed = await $.command.run({ ...commandInput, command: 'oberth-runs', args: '--limit=2' })
  expect(listed.text).toContain(rows[1]!.id.slice(0, 8))
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '--limit 2' })).text).toBe(listed.text)
  for (const row of rows) {
    expect((await $.command.run({ ...commandInput, command: 'oberth-watch', args: row.id })).text).toContain('Watching')
  }
  await noBox($)
  rows = rows.map(row => ({ ...row, status: 'passed' }))
  await clock.advance(15000)
  expect(notices).toEqual(rows.map(row => `Oberth ${row.id}: passed`))
  expect(calls.filter(call => call[2] === 'list')).toHaveLength(2)
  expect(calls.filter(call => call[2] === 'wait').map(call => call[3])).toEqual([rows[0]!.id])
  const finished = calls.length
  await clock.advance(180000)
  expect(calls).toHaveLength(finished)
  await noBox($)
})


// The first refresh outlives this test: its state calls are refused once the module is
// unloaded, and an unhandled rejection would fail the file after its last test.
// The test environment has timers; the module type environment declares none.
declare function setTimeout(callback: () => void, ms: number): unknown
test('a background refresh refused after unload stays handled', { options }, async ($, on) => {
  bottom(on)
  mock.clock(on)
  on('process.run', () => new Promise(resolve => setTimeout(() => resolve({ value: {
    exitCode: 1, stdout: '', stderr: 'unreachable', isStdoutTruncated: false, isStderrTruncated: false,
  } }), 50)))
  await $.session.start(start)
})

test('box hugs its rows with short names and no native names', { options }, async ($, on) => {
  bottom(on)
  mock.clock(on)
  const rows = [
    { ...run(), repo: 'origin/team/widget', ref: 'refs/tags/v1.2.0' },
    { ...run('aaaaaaaa000000000000000000000000', 'failed'), ref: 'refs/heads/main',
      failure: { failed_step: 'onExit[0].verify-release-artifacts', failed_step_name: 'verify-release-artifacts', node_id: 'generated-node-419' },
      steps: [{ name: 'verify-release-artifacts', pod_name: 'generated-pod-928' }] },
  ]
  cli(on, () => rows)
  await startSession($, on)
  for (const surface of ['terminal', 'desktop', 'mobile'] as const) {
    const ui = await $.ui.mount({ ...band, surface })
    const box = (await ui.findAll({ type: 'Box' })).find(box => box.props.borderStyle === 'round')
    expect(box?.props.alignSelf).toBe('flex-start')
    expect(box?.props.width).toBeUndefined()
    const lines = (await ui.findAll({ type: 'Text' })).map(line => line.text)
    expect(lines[1]).toMatch(/^cad7d9ec widget +v1\.2\.0 +▶ running /)
    expect(lines[2]).toMatch(/^aaaaaaaa oberth +main +✗ failed /)
    for (const line of lines) {
      for (const native of ['onExit[0]', 'generated-node-419', 'generated-pod-928', 'refs/']) expect(line).not.toContain(native)
      expect(line).toBe(line.trimEnd())
    }
    await ui.unmount()
  }
})

test('absent failed step has no native-name fallback', async () => {
  const item = { ...run(undefined, 'failed'), node_id: 'generated-node-419', pod_name: 'generated-pod-928',
    detail: 'generated-pod-928 failed', failure: null }
  const result = table([decodeRun(item)], 0, 100)
  expect(result).not.toContain('generated-node-419')
  expect(result).not.toContain('generated-pod-928')
  expect(() => decodeRun({ ...item, failure: { failed_step: 42, failed_step_name: 'test' } })).toThrow()
})

test('failed step name never falls back to the plan path', async () => {
  const item = { ...run(undefined, 'failed'), failure: { failed_step: 'onExit[0].cleanup', failed_step_name: '' } }
  expect(table([decodeRun(item)], 0, 100)).not.toContain('onExit[0].cleanup')
  expect(() => decodeRun({ ...item, failure: { failed_step: 'onExit[0].cleanup', failed_step_name: 42 } })).toThrow()
})

// `run list --limit 3 --json` of a v0.30.7 server with one running run, byte for byte as the CLI prints it.
const V0307_LIST = '{"runs":[{"id":"d7a7d4380c1e4b5f9a2b3c4d5e6f7a81","repo":"192-168-86-247/repos/oberth-smoke",' +
  '"ref":"refs/heads/smoke-2","object_sha":"5c1f0e9a7b3d2c4e6f8a0b1c2d3e4f5a6b7c8d9e",' +
  '"commit_sha":"5c1f0e9a7b3d2c4e6f8a0b1c2d3e4f5a6b7c8d9e","status":"running","display_reason":"step running",' +
  '"created_at":"2026-11-22T03:58:10.123456Z","started_at":null,"finished_at":null,"step_count":1,' +
  '"steps_planned_exact":true,"pending":0,"queued":0,"running":1,"passed":0,"failed":0,"skipped":0,"unknown":0,' +
  '"actor":"192.168.64.1","trigger":"push","schedule_entry":"","assist_requested":false,"rerun_of":""},' +
  '{"id":"0b4e2a91d3c54f6e8a7b9c0d1e2f3a41","repo":"192-168-86-247/repos/oberth-smoke","ref":"refs/heads/smoke-1",' +
  '"object_sha":"1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b","commit_sha":"1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b",' +
  '"status":"passed","display_reason":"passed","created_at":"2026-11-22T03:33:34Z","started_at":null,' +
  '"finished_at":"2026-11-22T03:33:54Z","step_count":1,"steps_planned_exact":true,"pending":0,"queued":0,' +
  '"running":0,"passed":1,"failed":0,"skipped":0,"unknown":0,"actor":"192.168.64.1","trigger":"push",' +
  '"schedule_entry":"","assist_requested":false,"rerun_of":""}],"next_cursor":""}\n'

function v0307(on: On) {
  const calls: { argv: readonly string[]; cwd?: string; env?: unknown }[] = []
  on('process.run', (_, e) => {
    calls.push({ argv: e.argv, cwd: e.init?.cwd, env: e.init?.env })
    const git = e.argv[0] === 'git'
    return { value: { exitCode: 0, stdout: git ? '192.168.64.1\n' : V0307_LIST, stderr: '',
      isStdoutTruncated: false, isStderrTruncated: false } }
  })
  return calls
}

const SMOKE = /^d7a7d438 oberth-smoke smoke-2 ▶ running +0\/1 +\S+ +\S+$/

test('a v0.30.7 run list reply with one running run draws the box', { options }, async ($, on) => {
  bottom(on)
  mock.clock(on, { now: Date.parse('2026-11-22T04:00:10Z') })
  v0307(on)
  await startSession($, on)
  for (const surface of ['terminal', 'desktop', 'mobile'] as const) {
    const ui = await $.ui.mount({ ...band, surface })
    const lines = await ui.findAll({ type: 'Text' })
    expect(lines.map(line => line.text)).toEqual([
      'RUN      REPO         REF     STATUS    STEPS TIME AGE',
      'd7a7d438 oberth-smoke smoke-2 ▶ running 0/1   —    1m',
      '0b4e2a91 oberth-smoke smoke-1 ✓ passed  1/1   —    26m',
    ])
    expect(SMOKE.test(lines[1]!.text)).toBe(true)
    await ui.unmount()
  }
})

// The joined bridge is MCP server "oberth", the name the host would serve this plugin's tools under;
// the kit's host refuses any, as a session with the bridge does.
test('the plugin registers no model tools and draws the box beside the oberth bridge', { options }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on, { now: Date.parse('2026-11-22T04:00:10Z') })
  const calls = v0307(on)
  await startSession($, on)
  await startSession($, on)
  expect(registeredTools.get(on)).toEqual([])
  await text($, SMOKE)
  await clock.advance(15000)
  expect(calls.filter(call => call.argv[2] === 'list')).toHaveLength(3)
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text).toContain('d7a7d438 oberth-smoke smoke-2 ▶ running')
})

test('blank, null and numeric-text runs options', async () => {
  for (const value of [undefined, null, '', '  ']) expect(shown(value)).toBe(3)
  expect(shown(0)).toBe(0)
  expect(shown('0')).toBe(0)
  expect(shown(' 7 ')).toBe(7)
  for (const value of [11, -1, 2.5, 'three', '11', true]) expect(shown(value)).toBeUndefined()
})

test('with no options the box uses the fetcher install and the checkout binding', async ($, on) => {
  bottom(on, undefined, true, undefined, { home: '/Users/person/' })
  mock.clock(on, { now: Date.parse('2026-11-22T04:00:10Z') })
  const calls = v0307(on)
  await startSession($, on)
  expect(calls[0]).toEqual({ argv: ['git', 'config', '--local', '--get', 'oberth.uplinkProfile'], cwd: '/checkout', env: undefined })
  const listed = calls.find(call => call.argv[2] === 'list')!
  expect(listed.argv).toEqual(['/Users/person/.local/bin/oberth', 'run', 'list', '--limit', '3', '--json'])
  expect(listed.env).toEqual({ OBERTH_PROFILE: '192.168.64.1' })
  await text($, SMOKE)
})

test('blank options fall back the same way; OBERTH_PROFILE wins over the binding', { options: { executable: '', profile: '' } }, async ($, on) => {
  bottom(on, undefined, true, 'env-profile', { home: '/Users/person' })
  mock.clock(on)
  const calls = v0307(on)
  await startSession($, on)
  expect(calls.some(call => call.argv[0] === 'git')).toBe(false)
  expect(calls[0]!.argv[0]).toBe('/Users/person/.local/bin/oberth')
  expect(calls[0]!.env).toEqual({ OBERTH_PROFILE: 'env-profile' })
})

test('a missing default executable notifies once and never runs it', async ($, on) => {
  bottom(on, undefined, true, 'env-profile', { home: '/Users/person', missing: '/Users/person/.local/bin/oberth' })
  mock.clock(on)
  const notices = toasts(on)
  const calls = v0307(on)
  await startSession($, on)
  await startSession($, on)
  expect(notices).toEqual([SETUP])
  expect(calls).toEqual([])
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text).toBe(SETUP)
  await noBox($)
})
