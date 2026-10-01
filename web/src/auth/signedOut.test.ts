import { describe, expect, it } from "vitest"
import { RELOAD_WINDOW_MS, mayAutoReload } from "./signedOut"

const now = 1_800_000_000_000

describe("mayAutoReload", () => {
  it("allows the first reload", () => {
    expect(mayAutoReload(null, now)).toBe(true)
  })

  it("refuses a second reload inside the window", () => {
    expect(mayAutoReload(String(now), now)).toBe(false)
    expect(mayAutoReload(String(now - 1), now)).toBe(false)
    expect(mayAutoReload(String(now - RELOAD_WINDOW_MS + 1), now)).toBe(false)
  })

  it("allows a reload once the window has passed", () => {
    expect(mayAutoReload(String(now - RELOAD_WINDOW_MS), now)).toBe(true)
    expect(mayAutoReload(String(now - 10 * RELOAD_WINDOW_MS), now)).toBe(true)
  })

  it("ignores a stored value that is not a past timestamp", () => {
    expect(mayAutoReload("garbage", now)).toBe(true)
    expect(mayAutoReload(String(now + 5000), now)).toBe(true)
  })
})
