import BreadcrumbGroup from "@cloudscape-design/components/breadcrumb-group"
import Flashbar from "@cloudscape-design/components/flashbar"
import { useEffect, useState } from "react"
import { Link, useLocation, useNavigate } from "react-router-dom"
import { useMe } from "./auth/MeProvider"
import { tokensAvailable } from "./policyApi"

export { api } from "./api"

declare global {
  interface Window {
    // Served by the backend at /config.js, so one build works in every environment.
    __BROWSERJS_CFG__?: { signOutUrl?: string }
  }
}

export interface Crumb {
  text: string
  href: string
}

// A message carried to the next page in the navigation's state: "Session
// brave-otter created" on the page the create flow lands on.
export interface Flash {
  type: "success" | "info" | "error"
  content: string
}

export const ROOT_CRUMB: Crumb = { text: "browserjs sessions", href: "/" }

interface ShellProps {
  children: React.ReactNode
  breadcrumbs?: Crumb[] // after the root; the last one is the current page
}

export function Shell({ children, breadcrumbs }: ShellProps) {
  return (
    <>
      <ShellHeader />
      <main className="wf-main">
        <ShellBody breadcrumbs={breadcrumbs}>{children}</ShellBody>
      </main>
    </>
  )
}

export function ShellHeader() {
  const me = useMe()
  const [tokens, setTokens] = useState(false)

  // The link to the tokens page appears only where the backend has tokens.
  useEffect(() => {
    let cancelled = false
    void tokensAvailable().then(ok => !cancelled && setTokens(ok))
    return () => {
      cancelled = true
    }
  }, [])

  return (
    <header className="wf-header">
      <h1>
        <Link to="/">browserjs sessions</Link>
      </h1>
      <span>
        {tokens && (
          <>
            <Link to="/tokens">API tokens</Link> ·{" "}
          </>
        )}
        {me.email} · <a href={window.__BROWSERJS_CFG__?.signOutUrl ?? "/.pomerium/sign_out"}>sign out</a>
      </span>
    </header>
  )
}

export function ShellBody({ children, breadcrumbs }: ShellProps) {
  return (
    <>
      {breadcrumbs && <Breadcrumbs items={[ROOT_CRUMB, ...breadcrumbs]} />}
      <PageFlash />
      {children}
    </>
  )
}

function Breadcrumbs({ items }: { items: Crumb[] }) {
  const navigate = useNavigate()
  return (
    <div className="wf-crumbs">
      <BreadcrumbGroup
        items={items}
        ariaLabel="Breadcrumbs"
        onFollow={e => {
          e.preventDefault()
          navigate(e.detail.href)
        }}
      />
    </div>
  )
}

function PageFlash() {
  const location = useLocation()
  const navigate = useNavigate()
  const flash = (location.state as { flash?: Flash } | null)?.flash
  if (!flash) return null
  return (
    <div className="wf-flash">
      <Flashbar
        items={[
          {
            type: flash.type,
            content: flash.content,
            dismissible: true,
            dismissLabel: "Dismiss",
            // Dropped from the history entry, so going back does not repeat it.
            onDismiss: () => navigate(location.pathname + location.search, { replace: true, state: null }),
            id: "flash",
          },
        ]}
      />
    </div>
  )
}

export function StateTag({ state }: { state: string }) {
  return (
    <span className="wf-state" data-state={state}>
      {state}
    </span>
  )
}
