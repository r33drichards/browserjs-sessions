import { describe, expect, it } from "vitest"
import { shouldRedirectToLogin } from "./session"

const now = 1_800_000_000_000 // ms
const nowS = now / 1000

describe("shouldRedirectToLogin", () => {
  it("redirects when there is no refresh token", () => {
    expect(shouldRedirectToLogin({}, now)).toBe(true)
    expect(shouldRedirectToLogin({ refreshToken: "", refreshExp: nowS + 600 }, now)).toBe(true)
  })

  it("does not redirect while the refresh token is still valid", () => {
    expect(shouldRedirectToLogin({ refreshToken: "r", refreshExp: nowS + 600 }, now)).toBe(false)
  })

  it("redirects once the refresh token has expired", () => {
    expect(shouldRedirectToLogin({ refreshToken: "r", refreshExp: nowS - 1 }, now)).toBe(true)
    expect(shouldRedirectToLogin({ refreshToken: "r", refreshExp: nowS }, now)).toBe(true)
  })

  it("allows for clock skew between the browser and Keycloak", () => {
    // Local clock runs 120s ahead of the server: a token the server issued
    // to expire 60s "ago" by local time is still good for another minute.
    expect(shouldRedirectToLogin({ refreshToken: "r", refreshExp: nowS - 60, timeSkew: 120 }, now)).toBe(false)
    // Local clock runs 120s behind: a token that looks 60s from expiry is already dead.
    expect(shouldRedirectToLogin({ refreshToken: "r", refreshExp: nowS + 60, timeSkew: -120 }, now)).toBe(true)
  })

  it("treats a refresh token without an expiry as still valid", () => {
    expect(shouldRedirectToLogin({ refreshToken: "r" }, now)).toBe(false)
    expect(shouldRedirectToLogin({ refreshToken: "r", refreshExp: 0, timeSkew: null }, now)).toBe(false)
  })
})
