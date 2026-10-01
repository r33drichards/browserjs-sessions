import { describe, expect, it } from "vitest"
import { reconnectDelay } from "./backoff"

describe("reconnectDelay", () => {
  it("doubles from 2s and caps at 30s", () => {
    expect([0, 1, 2, 3, 4, 5, 50].map(reconnectDelay)).toEqual([2000, 4000, 8000, 16000, 30000, 30000, 30000])
  })

  it("stays finite for very large attempt counts", () => {
    expect(reconnectDelay(5000)).toBe(30000)
  })
})
