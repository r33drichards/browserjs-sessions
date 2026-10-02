import { useEffect } from "react"

// Calls `load` once on mount and then every `intervalMs` while the page is
// visible. A hidden tab stops polling, or with `hiddenIntervalMs` carries on
// at that slower rate (a session's state should not go stale behind another
// tab); coming back loads immediately.
export function usePolling(load: () => void, enabled = true, intervalMs = 3000, hiddenIntervalMs?: number) {
  useEffect(() => {
    if (!enabled) return
    let timer: ReturnType<typeof setInterval> | undefined

    const stop = () => {
      clearInterval(timer)
      timer = undefined
    }
    // The timer for the page as it is now: the slow one, or none, while hidden.
    const start = () => {
      stop()
      const every = document.visibilityState === "hidden" ? hiddenIntervalMs : intervalMs
      if (every !== undefined) timer = setInterval(load, every)
    }
    const onVisibility = () => {
      if (document.visibilityState !== "hidden") load()
      start()
    }

    load()
    start()
    document.addEventListener("visibilitychange", onVisibility)
    return () => {
      stop()
      document.removeEventListener("visibilitychange", onVisibility)
    }
  }, [load, enabled, intervalMs, hiddenIntervalMs])
}

// For a session's state and its policy's: slow, not stopped, behind another tab.
export const HIDDEN_POLL_MS = 5000
