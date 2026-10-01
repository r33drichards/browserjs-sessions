import { Link } from "react-router-dom"
import { createApi } from "./api"
import { getToken, kc, username } from "./auth/keycloak"

export const api = createApi(getToken)

export function Shell({ children }: { children: React.ReactNode }) {
  return (
    <>
      <header className="wf-header">
        <h1>
          <Link to="/">browserjs sessions</Link>
        </h1>
        <span>
          {username()} · <button onClick={() => kc.logout({ redirectUri: window.location.origin })}>sign out</button>
        </span>
      </header>
      <main className="wf-main">{children}</main>
    </>
  )
}

export function StateTag({ state }: { state: string }) {
  return (
    <span className="wf-state" data-state={state}>
      {state}
    </span>
  )
}
