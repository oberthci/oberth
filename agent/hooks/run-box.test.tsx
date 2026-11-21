// Rendering is a manual gate: run `claude plugin test ./agent`; the build image has no host JavaScript runtime.
import { expect, mock, test } from 'claude-code/testing'
import type { Engine } from 'claude-code/testing'
import type { On } from 'claude-code'
import type { Run, Snapshot } from '../types'
import { decodeRun, progress, shortID, table, visible } from './model'

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

function bottom(on: On, resolved?: string, checkout = true, profileEnv?: string) {
  on('env.get', { name: 'OBERTH_PROFILE' }, () => ({ value: profileEnv }))
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
  on('tool.register', (_, e) => ({ value: { tool: `mcp__oberth__${e.name}` } }))
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('session.end', () => ({ sessionId: 'ended' }))
  on('session.root', () => ({ value: '/checkout' }))
  on('fs.stat', (_, e) => {
    if (e.path.endsWith('/.git') && !checkout) throw new Error('not found')
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

test('unset executable refuses commands and tools without spawning', async ($, on) => {
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
  expect((await $.tool.call({ tool: 'mcp__oberth__runs' })).isError).toBe(true)
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

test('unset profile stays hidden, notifies once and never spawns', { options: { executable: options.executable } }, async ($, on) => {
  bottom(on)
  const clock = mock.clock(on)
  const toasts: string[] = []
  on('ui.toast', (_, e) => { toasts.push(e.text); return { value: undefined } })
  on('process.run', () => { throw new Error('must not spawn with the current profile') })
  await startSession($, on)
  await startSession($, on)
  await clock.advance(120000)
  expect(toasts.length).toBe(1)
  expect(toasts[0]).toContain('Set profile to the uplink profile')
  expect((await $.tool.call({ tool: 'mcp__oberth__runs' })).isError).toBe(true)
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
  expect((await $.tool.call({ tool: 'mcp__oberth__watch', id: 'bbbbbbbb' })).result).toContain('Watching')
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

test('unreachable and truncated responses hide stale rows; tools return plain failures', { options }, async ($, on) => {
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
  const failed = await $.tool.call({ tool: 'mcp__oberth__runs' })
  expect(failed.isError).toBe(true)
  expect(failed.result).toBe(stderr)
  truncatedDiagnostic = true
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text).toContain('output limit')
  truncatedDiagnostic = false
  exitCode = 0
  truncated = true
  expect((await $.command.run({ ...commandInput, command: 'oberth-runs', args: '' })).text).toContain('output limit')
})

test('table command and model tool keep complete IDs, refs and reasons', { options }, async ($, on) => {
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
  const tool = await $.tool.call({ tool: 'mcp__oberth__runs', limit: 7 })
  expect(command.text).toBe(tool.result)
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
  expect(visible({ recent: [first], pins: [], now: 0, reachable: false })).toEqual([])
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
