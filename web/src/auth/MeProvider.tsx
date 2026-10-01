// Gates rendering on GET /api/me. The identity proxy has already signed the
// user in by the time the page loads, so this only asks who they are.
import { createContext, useCallback, useContext, useEffect, useState, useSyncExternalStore } from "react"
import type { Me } from "../api"
import { api } from "../api"
import { isSignedOut, signedOutHandled, subscribeSignedOut } from "./signedOut"

const MeContext = createContext<Me | null>(null)

export function useMe(): Me {
  const me = useContext(MeContext)
  if (!me) throw new Error("useMe outside MeProvider")
  return me
}

export function MeProvider({ children }: { children: React.ReactNode }) {
  const [me, setMe] = useState<Me | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [attempt, setAttempt] = useState(0)
  const signedOut = useSyncExternalStore(subscribeSignedOut, isSignedOut)

  useEffect(() => {
    let cancelled = false
    api
      .me()
      .then(result => {
        if (!cancelled) setMe(result)
      })
      .catch(e => {
        if (cancelled || signedOutHandled(e)) return
        setError(String(e instanceof Error ? e.message : e))
      })
    return () => {
      cancelled = true
    }
  }, [attempt])

  const retry = useCallback(() => {
    setError(null)
    setAttempt(n => n + 1)
  }, [])

  if (signedOut) {
    return (
      <main className="auth-error">
        <h1>You&apos;ve been signed out</h1>
        <button type="button" onClick={() => window.location.reload()}>
          Sign in
        </button>
      </main>
    )
  }
  if (error) {
    return (
      <main className="auth-error">
        <h1>We couldn&apos;t load the app</h1>
        <code>{error}</code>
        <button type="button" onClick={retry}>
          Try again
        </button>
      </main>
    )
  }
  if (!me) return null
  return <MeContext.Provider value={me}>{children}</MeContext.Provider>
}
