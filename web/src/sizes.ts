// A session's size: what the desktop may use. The sizes a deployment offers
// come from GET /api/sizes; nothing here knows a number.

export interface SizeInfo {
  name: string // "small", "medium", "large"
  cpuMillis: number // the desktop's CPU limit
  memoryMiB: number // the desktop's memory limit
  warm: boolean // a new session of this size is one that is already running
}

export interface Sizes {
  default: string
  sizes: SizeInfo[]
}

const trim = (n: number) => String(Math.round(n * 10) / 10)

export const sizeLabel = (name: string) => name.charAt(0).toUpperCase() + name.slice(1)

// "1.5 CPU, 2 GiB of memory"
export function sizeNumbers(s: SizeInfo): string {
  return `${trim(s.cpuMillis / 1000)} CPU, ${trim(s.memoryMiB / 1024)} GiB of memory`
}

// How long a new session of this size takes to be there.
export function sizeStart(s: SizeInfo): string {
  return s.warm
    ? "Ready in a few seconds."
    : "Starts cold: well under a minute where a machine has room, about two minutes when one has to be started."
}

// What resizing does to a session, said before it is done.
export function resizeConsequence(state: string): string {
  return state === "running" || state === "starting"
    ? "The session keeps running at the size it has. It has the new size from its next start (after a sleep or a stop), and that start is a fresh one: open windows and anything not saved to its disk are lost. Its files are kept."
    : "The session starts at the new size the next time it starts, and that start is a fresh one: if its state was saved when it went to sleep, that state is dropped. Its files are kept."
}
