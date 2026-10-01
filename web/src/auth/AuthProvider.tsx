// Gates rendering until Keycloak login resolves. With onLoad: "login-required"
// the user is redirected to Keycloak before the app mounts.
import { useEffect, useState } from "react"
import { initKc } from "./keycloak"

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [ready, setReady] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    initKc()
      .then(authed => (authed ? setReady(true) : setError("Not authenticated")))
      .catch(e => setError(String(e)))
  }, [])

  if (error) {
    return (
      <main className="auth-error">
        <h1>We couldn&apos;t sign you in</h1>
        <code>{error}</code>
        <button type="button" onClick={() => window.location.reload()}>
          Try again
        </button>
      </main>
    )
  }
  if (!ready) return null
  return <>{children}</>
}
