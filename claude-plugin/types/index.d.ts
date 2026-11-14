export type OberthWatch = { id: string; line: string; status: string; done: boolean; failures: number }
declare module 'claude-code' {
  interface PluginState {
    oberth: { watch: Shaped<OberthWatch | null> }
  }
}
