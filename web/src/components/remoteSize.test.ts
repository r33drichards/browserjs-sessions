import { describe, expect, it } from "vitest"
import { remoteSize } from "./remoteSize"

describe("remoteSize", () => {
  it("is the box itself, in whole pixels", () => {
    expect(remoteSize(1156, 721)).toEqual({ w: 1156, h: 721 })
    expect(remoteSize(1156.5, 721.9)).toEqual({ w: 1156, h: 721 })
  })

  it("keeps the shape of a box over the limit", () => {
    expect(remoteSize(3840, 2160)).toEqual({ w: 2560, h: 1440 })
    expect(remoteSize(2000, 3200)).toEqual({ w: 1000, h: 1600 })
    expect(remoteSize(2560, 1600)).toEqual({ w: 2560, h: 1600 })
  })

  it("asks for nothing when the box is too small to be a desktop", () => {
    expect(remoteSize(0, 0)).toBeNull()
    expect(remoteSize(1280, 40)).toBeNull()
    expect(remoteSize(NaN, 800)).toBeNull()
  })

  it("never goes under the minimum for a very long box", () => {
    expect(remoteSize(100000, 200)).toEqual({ w: 2560, h: 200 })
  })
})
