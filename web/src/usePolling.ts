import { useEffect } from "react"

// Calls `load` once on mount and then every `intervalMs` while the page is
// visible. A hidden tab stops polling; coming back loads immediately.
export function usePolling(load: () => void, enabled = true, intervalMs = 3000) {
  useEffect(() => {
    if (!enabled) return
    let timer: ReturnType<typeof setInterval> | undefined

    const start = () => {
      if (timer === undefined) timer = setInterval(load, intervalMs)
    }
    const stop = () => {
      clearInterval(timer)
      timer = undefined
    }
    const onVisibility = () => {
      if (document.visibilityState === "hidden") return stop()
      if (timer !== undefined) return
      load()
      start()
    }

    load()
    if (document.visibilityState !== "hidden") start()
    document.addEventListener("visibilitychange", onVisibility)
    return () => {
      stop()
      document.removeEventListener("visibilitychange", onVisibility)
    }
  }, [load, enabled, intervalMs])
}
