import { expect, test } from 'claude-code/testing'
import { age, clean, clip, duration, limit, statusLine, table, watchLine } from './format'
import type { Run } from './format'

const now = Date.parse('2026-11-14T12:00:00Z')
const base: Run = { id: '1234567890', repo: 'oberth', ref: 'main', status: 'passed', created_at: '2026-11-14T10:00:00Z' }

test('finished duration uses the step extrema and never the wall clock', () => {
  const r = { ...base, steps: [
    { started_at: '2026-11-14T10:02:00Z', finished_at: '2026-11-14T10:04:00Z' },
    { started_at: '2026-11-14T10:00:00Z', finished_at: '2026-11-14T10:06:00Z' },
  ] }
  expect(duration(r, now)).toBe('6m')
  expect(duration(r, now + 86400000)).toBe('6m')
  expect(duration({ ...r, started_at: '2026-11-14T10:01:00Z', finished_at: '2026-11-14T10:03:00Z' }, now)).toBe('2m')
})

test('running and missing timestamps have honest durations', () => {
  expect(duration({ ...base, status: 'running', started_at: '2026-11-14T10:00:00Z' }, now)).toBe('2h00')
  expect(duration(base, now)).toBe('—')
  expect(duration({ ...base, started_at: '2026-11-14T10:00:00Z' }, now)).toBe('—')
  expect(duration({ ...base, status: 'running', steps: [{ started_at: 'bad' }] }, now)).toBe('—')
  expect(duration({ ...base, started_at: '1970-01-01T00:00:00Z', finished_at: '1970-01-01T00:00:02Z' }, now)).toBe('2s')
  expect(duration({ ...base, started_at: '2026-11-14T10:00:00Z', finished_at: '2026-11-14T09:00:00Z' }, now)).toBe('—')
})

test('controls, terminal strings and Unicode cannot move columns', () => {
  const hostile = '\x1b[31mred\x1b[0m\x1b]0;title\x07\x1bPpayload\x1b\\\n\r\t\u202Eref\x9b2J'
  expect(clean(hostile)).toBe('?red???????ref?')
  expect(clip(hostile, 5)).toBe('?red…')
  expect(clip('界e\u0301😀', 10)).toBe('?e??')
  expect(clean('x\x1b]unfinished')).toBe('x?')
  expect(clean('x\x1bPunfinished')).toBe('x?')
})

test('fixed-width table includes every column and truncates untrusted fields', () => {
  const lines = table([{ ...base, repo: 'oberth'.repeat(9), failed_step: '\x1b[2Junit\nfail', ref: 'x'.repeat(30) }], now).split('\n')
  expect(lines.length).toBe(2)
  expect(lines[0]!).toContain('FAILED STEP')
  expect(lines[0]!.length).toBe(103)
  expect(lines[1]!.length).toBe(lines[0]!.length)
  expect(lines[1]!).toContain('unit?fail')
  expect(lines[1]!.indexOf('✓ passed')).toBe(50)
  expect(table([], now)).toBe('No runs.')
  expect(age(base, now)).toBe('2h ago')
  expect(limit(999)).toBe(50)
  expect(limit(undefined)).toBe(5)
})

test('status and watch lines are read-only summaries', () => {
  expect(statusLine([{ ...base, status: 'running' }, base])).toBe('Oberth ▸ 1 running · last ✓ main')
  expect(watchLine({ ...base, failed_step: 'unit\nfail' }, now)).toContain('unit?fail')
})


test('unterminated C1 control strings remain visibly distinct from a trusted ref', () => {
  for (const control of ['\u0090', '\u0098', '\u009d', '\u009e', '\u009f']) {
    expect(clean(`main${control}-evil-suffix`)).toBe('main?')
  }
  expect(clean('main\x1b]title\x07-suffix')).toBe('main?-suffix')
  expect(clean('main\x1bPpayload\x1b\\-suffix')).toBe('main?-suffix')
})
