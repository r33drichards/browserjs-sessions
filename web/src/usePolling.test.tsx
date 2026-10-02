// @vitest-environment jsdom
import { cleanup, renderHook } from "@testing-library/react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { HIDDEN_POLL_MS, usePolling } from "./usePolling"

let visibility: DocumentVisibilityState

function show(state: DocumentVisibilityState) {
  visibility = state
  document.dispatchEvent(new Event("visibilitychange"))
}

beforeEach(() => {
  visibility = "visible"
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => visibility })
  vi.useFakeTimers()
})

afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

describe("usePolling", () => {
  it("loads at once and then at its interval", () => {
    const load = vi.fn()
    renderHook(() => usePolling(load))
    expect(load).toHaveBeenCalledTimes(1)
    vi.advanceTimersByTime(9000)
    expect(load).toHaveBeenCalledTimes(4)
  })

  it("stops while the tab is hidden, and loads at once when it is back", () => {
    const load = vi.fn()
    renderHook(() => usePolling(load))
    show("hidden")
    vi.advanceTimersByTime(60_000)
    expect(load).toHaveBeenCalledTimes(1)
    show("visible")
    expect(load).toHaveBeenCalledTimes(2)
    vi.advanceTimersByTime(3000)
    expect(load).toHaveBeenCalledTimes(3)
  })

  it("carries on slowly while hidden when given a hidden interval", () => {
    const load = vi.fn()
    renderHook(() => usePolling(load, true, 3000, HIDDEN_POLL_MS))
    show("hidden")
    vi.advanceTimersByTime(2 * HIDDEN_POLL_MS)
    expect(load).toHaveBeenCalledTimes(3) // the first, then one every 5 s: not every 3
    show("visible")
    expect(load).toHaveBeenCalledTimes(4) // without waiting for a tick
    vi.advanceTimersByTime(6000)
    expect(load).toHaveBeenCalledTimes(6) // and back to every 3 s
  })

  it("polls slowly from the start in a tab opened in the background", () => {
    visibility = "hidden"
    const load = vi.fn()
    renderHook(() => usePolling(load, true, 3000, HIDDEN_POLL_MS))
    vi.advanceTimersByTime(HIDDEN_POLL_MS)
    expect(load).toHaveBeenCalledTimes(2)
  })

  it("does nothing when not enabled, and stops when unmounted", () => {
    const off = vi.fn()
    renderHook(() => usePolling(off, false))
    const load = vi.fn()
    const { unmount } = renderHook(() => usePolling(load))
    unmount()
    vi.advanceTimersByTime(30_000)
    expect(off).not.toHaveBeenCalled()
    expect(load).toHaveBeenCalledTimes(1)
  })
})
