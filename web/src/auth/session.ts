// What keycloak-js knows about the refresh token after a failed refresh.
export interface RefreshState {
  refreshToken?: string
  refreshExp?: number // seconds since the epoch, Keycloak's clock
  timeSkew?: number | null // seconds the local clock runs ahead of Keycloak's
}

// A failed token refresh warrants a full login redirect only when the Keycloak
// session is really gone: no refresh token, or one that has expired. Anything
// else is treated as transient.
export function shouldRedirectToLogin(state: RefreshState, nowMs: number): boolean {
  if (!state.refreshToken) return true
  if (!state.refreshExp) return false // offline tokens carry no expiry
  return state.refreshExp + (state.timeSkew ?? 0) <= Math.ceil(nowMs / 1000)
}
