// Rendering is a manual gate: run `claude plugin test ./agent`; the build image has no host JavaScript runtime.
import { expect, mock, test } from 'claude-code/testing'
import type { Engine } from 'claude-code/testing'
import type { On } from 'claude-code'
import type { Run, Snapshot } from '../types'
import { decodeRun, progress, shortID, shown, SHOWN_REASON, SHOWN_REMEDY, table, visible } from './model'
import { SETUP } from './client'

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
  on('fs.stat', (_, e) => {
    if (e.path.endsWith('/.git') && !checkout || e.path === host.missing) throw new Error('not found')
    return { value: {
      kind: e.path === '/checkout' ? 'dir' : 'file',
      realPath: e.path === '/checkout' ? e.path : resolved ?? e.path,
      size: 1, mtimeMs: 0, isLink: false,
    } }
  })
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
    value: { shape: 'runs-1', value: { recent: [run()], pins: [], reachable: true, now: 0 } }, version: 1,
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
  await text($, /▶ cad7d9ec/)
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
  await text($, /▶ cad7d9ec/)
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

test('one list per tick, whole fields, authoritative counts and disappearance', { options }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on, { now: Date.parse('2026-11-18T00:01:00Z') })
  const calls: string[][] = []
  const running = { ...run(), ref: 'refs/heads/a-ref-that-is-longer-than-any-fixed-column' }
  let rows = [running, run('a4c399fd000000000000000000000000', 'passed')]
  on('process.run', (_, e) => {
    calls.push([...e.argv])
    return { value: { exitCode: 0, stdout: JSON.stringify({ runs: rows }), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  await startSession($, on)
  expect(calls[0]).toEqual(['/verified/oberth', 'run', 'list', '--limit', '3', '--json'])
  await text($, /▶ cad7d9ec oberth refs\/heads\/a-ref-that-is-longer-than-any-fixed-column 6\/11 1m/)
  await clock.advance(60000)
  expect(calls.length).toBe(5)
  rows = rows.map(row => ({ ...row, status: 'passed' }))
  await clock.advance(15000)
  const ui = await $.ui.mount({ ...band, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /cad7d9ec/ })).toBeUndefined()
  await ui.unmount()
  const before = calls.length
  await clock.advance(60000)
  expect(calls.length - before).toBe(1)
  rows = [running]
  await clock.advance(60000)
  expect(calls.length - before).toBe(2)
  await text($, /▶ cad7d9ec/)
  await clock.advance(15000)
  expect(calls.length - before).toBe(3)
})

test('watching a run switches idle reads to the active interval', { options }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on)
  let lists = 0
  let gets = 0
  on('process.run', (_, e) => {
    const detail = e.argv[2] === 'get'
    if (detail) gets++
    else lists++
    return { value: { exitCode: 0, stdout: JSON.stringify(detail ? run() : { runs: [] }),
      stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  await startSession($, on)
  await clock.advance(45000)
  expect(lists).toBe(1)
  await $.command.run({ ...commandInput, command: 'oberth-watch', args: run().id })
  await clock.advance(15000)
  expect(lists).toBe(2)
  expect(gets).toBe(2)
  await clock.advance(15000)
  expect(lists).toBe(3)
  expect(gets).toBe(3)
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
  expect(gets).toBe(5)
  await text($, /▶ bbbbbbbb oberth v0.24.0 6\/11 .* ●/)
  current = { ...pinned, status: 'failed', failed: 1, failure: { failed_step: 'onExit[0].lint', failed_step_name: 'lint', node_id: 'generated-node-419' } } as Run
  await clock.advance(15000)
  expect(toasts).toEqual([`Oberth ${pinned.id}: failed; step lint`])
  await clock.advance(60000)
  expect(gets).toBe(6)
  expect(toasts.length).toBe(1)
  const finished = await $.command.run({ ...commandInput, command: 'oberth-watch', args: pinned.id })
  expect(finished.text).toBe(`${pinned.id}: failed; step lint; already finished.`)
  expect(toasts.length).toBe(1)
})

test('unreachable and truncated responses hide stale rows; the table returns plain failures', { options }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on)
  let exitCode = 0
  let truncated = false
  let truncatedDiagnostic = false
  let stderr = ''
  on('process.run', () => ({ value: { exitCode, stdout: JSON.stringify({ runs: [run()] }), stderr, isStdoutTruncated: truncated, isStderrTruncated: truncatedDiagnostic } }))
  await startSession($, on)
  await text($, /▶ cad7d9ec/)
  exitCode = 1
  stderr = 'The run selector is ambiguous.\nUse the complete run ID from the run table.'
  await clock.advance(15000)
  const ui = await $.ui.mount({ ...band, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /cad7d9ec/ })).toBeUndefined()
  await ui.unmount()
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text).toBe(stderr)
  truncatedDiagnostic = true
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text).toContain('output limit')
  truncatedDiagnostic = false
  exitCode = 0
  truncated = true
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text).toContain('output limit')
})

test('table command keeps complete IDs, refs and reasons', { options }, async ($, on) => {
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
  expect(command.text).toContain(item.id)
  expect(command.text).toContain(item.display_reason)
  expect(command.text).toContain('FAILED STEP')
  expect(command.text).toContain('verify-release-artifacts')
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
  expect(visible({ recent: [first], pins: [], now: 0, reachable: false }, 3)).toEqual([])
  expect(table([{ ...first, ref: 'ref\u001b\u200b' }], 0)).not.toContain('\u001b')
  expect(table([{ ...first, ref: 'ref\u001b\u200b' }], 0)).not.toContain('\u200b')
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
  on('process.run', () => {
    calls++
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
    value: { shape: 'runs-1', value: { recent: [], pins: [], reachable: false, now: 0 } }, version: 0,
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
      expect(lines.map(line => line.text.split(' ')[1])).toEqual(six.slice(0, count).map(run => run.id.slice(0, 8)))
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
    expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text).toContain(six[5]!.id)
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
  expect(listed.text).toContain(rows[1]!.id)
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '--limit 2' })).text).toBe(listed.text)
  for (const row of rows) {
    expect((await $.command.run({ ...commandInput, command: 'oberth-watch', args: row.id })).text).toContain('Watching')
  }
  await noBox($)
  rows = rows.map(row => ({ ...row, status: 'passed' }))
  await clock.advance(15000)
  expect(notices).toEqual(rows.map(row => `Oberth ${row.id}: passed`))
  expect(calls.map(call => call[2])).toEqual(['list', 'list', 'get', 'get', 'get', 'get'])
  await clock.advance(180000)
  expect(calls).toHaveLength(6)
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

test('box fits whole repository and ref columns and names the failed plan step', { options }, async ($, on) => {
  bottom(on)
  mock.clock(on)
  const repository = 'origin/team/a-repository-name-that-exceeds-the-prototype-column-cap'
  const ref = 'refs/heads/a-branch-name-that-exceeds-the-prototype-ref-column-cap'
  const rows = [
    { ...run(), repo: 'short-repo', ref },
    { ...run('aaaaaaaa000000000000000000000000', 'failed'), repo: repository, ref: 'refs/heads/main',
      failure: { failed_step: 'onExit[0].verify-release-artifacts', failed_step_name: 'verify-release-artifacts', node_id: 'generated-node-419' },
      steps: [{ name: 'verify-release-artifacts', pod_name: 'generated-pod-928' }] },
  ]
  on('process.run', () => ({ value: { exitCode: 0, stdout: JSON.stringify({ runs: rows }),
    stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }))
  await startSession($, on)
  for (const surface of ['terminal', 'desktop', 'mobile'] as const) {
    const ui = await $.ui.mount({ ...band, surface })
    const box = await ui.find({ type: 'Box' })
    expect(box?.props.alignSelf).toBe('flex-start')
    expect(box?.props.width).toBeUndefined()
    const lines = await ui.findAll({ type: 'Text' })
    expect(lines.length).toBe(2)
    expect(lines[0]!.text).toContain(ref)
    expect(lines[1]!.text).toContain(repository)
    const refColumn = 2 + 8 + 1 + repository.length + 1
    expect(lines[0]!.text.indexOf(ref)).toBe(refColumn)
    expect(lines[1]!.text.indexOf('refs/heads/main')).toBe(refColumn)
    expect(lines[0]!.text.indexOf('6/11')).toBe(refColumn + ref.length + 1)
    expect(lines[1]!.text.indexOf('6/11')).toBe(refColumn + ref.length + 1)
    expect(lines[1]!.text).toContain('[failed: verify-release-artifacts]')
    for (const line of lines) {
      expect(line.props.wrap).toBe('wrap')
      expect(line.text).not.toContain('…')
      expect(line.text).not.toContain('onExit[0]')
      expect(line.text).not.toContain('generated-node-419')
      expect(line.text).not.toContain('generated-pod-928')
      expect(line.text).toBe(line.text.trimEnd())
    }
    await ui.unmount()
  }
})

test('absent failed step has no native-name fallback', async () => {
  const item = { ...run(undefined, 'failed'), node_id: 'generated-node-419', pod_name: 'generated-pod-928',
    detail: 'generated-pod-928 failed', failure: null }
  const result = table([decodeRun(item)], 0)
  expect(result).not.toContain('generated-node-419')
  expect(result).not.toContain('generated-pod-928')
  expect(() => decodeRun({ ...item, failure: { failed_step: 42, failed_step_name: 'test' } })).toThrow()
})

test('failed step name never falls back to the plan path', async () => {
  const item = { ...run(undefined, 'failed'), failure: { failed_step: 'onExit[0].cleanup', failed_step_name: '' } }
  expect(table([decodeRun(item)], 0)).not.toContain('onExit[0].cleanup')
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

const SMOKE = /^▶ d7a7d438 192-168-86-247\/repos\/oberth-smoke refs\/heads\/smoke-2 0\/1 \d+m$/

test('a v0.30.7 run list reply with one running run draws the box', { options }, async ($, on) => {
  bottom(on)
  mock.clock(on, { now: Date.parse('2026-11-22T04:00:10Z') })
  v0307(on)
  await startSession($, on)
  for (const surface of ['terminal', 'desktop', 'mobile'] as const) {
    const ui = await $.ui.mount({ ...band, surface })
    const lines = await ui.findAll({ type: 'Text' })
    expect(lines.map(line => line.text)).toHaveLength(2)
    expect(SMOKE.test(lines[0]!.text)).toBe(true)
    expect(lines[0]!.text.endsWith(' 2m')).toBe(true)
    expect(lines[1]!.text.startsWith('✓ 0b4e2a91 ')).toBe(true)
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
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text).toContain('d7a7d4380c1e4b5f9a2b3c4d5e6f7a81')
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
