import { Link } from "react-router-dom"
import { useMe } from "./auth/MeProvider"

export { api } from "./api"

declare global {
  interface Window {
    // Served by the backend at /config.js, so one build works in every environment.
    __BROWSERJS_CFG__?: { signOutUrl?: string }
  }
}

export function Shell({ children }: { children: React.ReactNode }) {
  const me = useMe()
  return (
    <>
      <header className="wf-header">
        <h1>
          <Link to="/">browserjs sessions</Link>
        </h1>
        <span>
          {me.email} · <a href={window.__BROWSERJS_CFG__?.signOutUrl ?? "/.pomerium/sign_out"}>sign out</a>
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
