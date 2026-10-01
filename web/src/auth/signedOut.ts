// What to do once the API reports that the proxy session is gone. A full page
// load sends the browser through the proxy, which redirects to sign-in; the
// guard keeps a proxy that never lets us back in from becoming a reload loop.
import { SignedOutError } from "../api"

export const RELOAD_WINDOW_MS = 30_000
const STORAGE_KEY = "browserjs.signedOutReloadAt"

// Whether an automatic reload is allowed, given the stored time of the last one
// (ms since the epoch, as a string; null when there was none).
export function mayAutoReload(lastReload: string | null, nowMs: number): boolean {
  if (lastReload === null) return true
  const last = Number(lastReload)
  if (!Number.isFinite(last)) return true
  const elapsed = nowMs - last
  return elapsed < 0 || elapsed >= RELOAD_WINDOW_MS // a timestamp from the future is a clock change
}

let signedOut = false
const listeners = new Set<() => void>()

export const isSignedOut = () => signedOut

export function subscribeSignedOut(listener: () => void) {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

// Reloads the page, at most once per window; past that, flips the app to its
// "signed out" box instead. Without sessionStorage there is no guard, so no
// automatic reload either.
export function handleSignedOut() {
  if (signedOut) return
  signedOut = true
  try {
    if (mayAutoReload(sessionStorage.getItem(STORAGE_KEY), Date.now())) {
      sessionStorage.setItem(STORAGE_KEY, String(Date.now()))
      window.location.reload()
      return
    }
  } catch {}
  listeners.forEach(listener => listener())
}

// For catch blocks: true when the error was a sign-out and has been dealt with.
export function signedOutHandled(error: unknown): boolean {
  if (!(error instanceof SignedOutError)) return false
  handleSignedOut()
  return true
}
