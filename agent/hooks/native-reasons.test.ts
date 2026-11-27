import { expect, test } from 'claude-code/testing'
import type { Run, Snapshot } from '../types'
import { boxRows, duration, table } from './model'

test('all native reasons survive metadata fitting and physical minimum widths', async () => {
  const now = Date.parse('2026-11-18T00:01:00Z')
  for (const reason of ['waiting for a run slot', 'waiting for a node', 'image pull failed', 'held for debugging', 'node memory pressure', 'waiting for quota']) {
    const run: Run = { id: 'cad7d9ec000000000000000000000000', repo: 'origin/team/long-repository', ref: 'refs/heads/long-topic',
      status: 'running', display_reason: reason, created_at: '2026-11-18T00:00:00Z', started_at: '2026-11-18T00:00:00Z', finished_at: null,
      step_count: 2, steps_planned_exact: true, passed: 0, failed: 0, skipped: 0, selected_file: '.oberth/check.yaml' }
    const state: Snapshot = { recent: [run], pins: [], now, reachable: true, singleProfile: true }
    for (const width of [8, 30, 40, 60, 80, 120]) {
      expect(boxRows(state, 3, width)[1]).toContain(reason)
      expect(table([run], now, width)).toContain(reason)
    }
  }
})

test('terminal duration is frozen through a long decision gap', async () => {
  const now = Date.parse('2026-11-18T00:00:20Z')
  const run = { started_at: '2026-11-17T23:59:59Z', created_at: '2026-11-18T00:00:00Z', finished_at: '2026-11-18T00:00:21Z' } as Run
  expect(duration(run, now)).toBe('22s')
  expect(duration(run, now + 48 * 3600000)).toBe('22s')
})
