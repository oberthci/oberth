// The public CLI projection consumed by the run box and its persisted state.
export type Failure = {
  failed_step: string
  failed_step_name: string
}

export type Run = {
  id: string
  repo: string
  ref: string
  selected_file?: string
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

// Local provenance; never sent to the CLI. Offset maps each server's clock to this host's.
export type ObservedRun = Run & { profile?: string; clockOffset?: number }
export type Pin = { run: ObservedRun; doneAt: number | null }
export type ProfileClock = { profile: string; offset: number }
export type Snapshot = { recent: ObservedRun[]; pins: Pin[]; reachable: boolean; now: number; singleProfile: boolean; unavailable?: string[]; clocks?: ProfileClock[] }

declare module 'claude-code' {
  interface PluginState {
    oberth: { snapshot: Shaped<Snapshot> }
  }
}
