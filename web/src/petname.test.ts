import { describe, expect, it } from "vitest"
import { petname } from "./petname"

describe("petname", () => {
  it("is two lowercase words joined by a hyphen", () => {
    for (let i = 0; i < 200; i++) expect(petname()).toMatch(/^[a-z]+-[a-z]+$/)
  })
  it("varies", () => {
    expect(new Set(Array.from({ length: 50 }, petname)).size).toBeGreaterThan(5)
  })
})
