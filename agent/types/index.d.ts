// The public CLI projection consumed by the run box and its persisted state.
export type Failure = {
  failed_step: string
  failed_step_name: string
}

export type Run = {
  id: string
  repo: string
  ref: string
  status: 'queued' | 'running' | 'passed' | 'failed' | 'cancelled'
  failure?: Failure | null
  display_reason: string
  created_at: string
  started_at: string | null
  finished_at: string | null
  step_count: number
  steps_planned_exact: boolean
  passed: number
  failed: number
  skipped: number
}

export type Pin = { run: Run; doneAt: number | null }
// now: the server's clock at the last refresh when the list carried it, else this host's.
export type Snapshot = { recent: Run[]; pins: Pin[]; reachable: boolean; now: number }

declare module 'claude-code' {
  interface PluginState {
    oberth: { snapshot: Shaped<Snapshot> }
  }
}
