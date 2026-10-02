// The account's billing state for every page: asked once, then every 15 s
// while the page is visible. Where the backend has billing off (GET
// /api/billing is a 404) the context says so and nothing of billing is drawn.
//
// Also here, because every page can start them: leaving for Stripe (a card, a
// pack, a plan, the portal), the add-credit modal, and following a Checkout
// the browser has come back from.
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react"
import { useLocation, useNavigate } from "react-router-dom"
import type { Session } from "../api"
import { ApiError, api } from "../api"
import { signedOutHandled } from "../auth/signedOut"
import type { Banner, Billing, BillingAction, Catalogue } from "../billingApi"
import { billingApi, browser, checkoutMessage, followCheckout, isCheckoutId, planChangeMessage } from "../billingApi"
import { usePolling } from "../usePolling"
import { AddCreditModal } from "./AddCreditModal"

export interface BillingContext {
  // undefined while first asking; null where the backend has billing off.
  billing: Billing | null | undefined
  catalogue: Catalogue | null
  sessions: Session[] // the caller's own; empty until loaded
  refused: "account_blocked" | "signups_paused" | null // GET /api/billing answered 403
  reload: () => void
  busy: BillingAction | null
  run: (action: BillingAction) => void
  subscribe: (lookupKey: string) => void
  changePlan: (lookupKey: string, name: string) => void // for a subscriber: no Checkout, no portal
  notices: Banner[] // the checkout's outcome, and a payment page that would not open
  dismiss: (id: string) => void
}

const OFF: BillingContext = {
  billing: null,
  catalogue: null,
  sessions: [],
  refused: null,
  reload: () => {},
  busy: null,
  run: () => {},
  subscribe: () => {},
  changePlan: () => {},
  notices: [],
  dismiss: () => {},
}

const Context = createContext<BillingContext>(OFF)

// Outside a provider (a page rendered alone) billing is off.
export const useBilling = () => useContext(Context)

const BEFORE_KEY = "browserjs.checkoutBalanceBefore"
const STRIPE_DOWN = "The payment page could not be opened. Nothing was charged. Try again."

function remember(balanceMicros: number) {
  try {
    sessionStorage.setItem(BEFORE_KEY, String(balanceMicros))
  } catch {}
}

function recall(): number | undefined {
  try {
    const stored = sessionStorage.getItem(BEFORE_KEY)
    sessionStorage.removeItem(BEFORE_KEY)
    return stored === null || !Number.isFinite(Number(stored)) ? undefined : Number(stored)
  } catch {
    return undefined
  }
}

export function BillingProvider({ children }: { children: React.ReactNode }) {
  const navigate = useNavigate()
  const location = useLocation()
  const [billing, setBilling] = useState<Billing | null | undefined>(undefined)
  const [catalogue, setCatalogue] = useState<Catalogue | null>(null)
  const [sessions, setSessions] = useState<Session[]>([])
  const [refused, setRefused] = useState<BillingContext["refused"]>(null)
  const [busy, setBusy] = useState<BillingAction | null>(null)
  const [notices, setNotices] = useState<Banner[]>([])
  const [adding, setAdding] = useState(false)
  const catalogueRef = useRef<Catalogue | null>(null)
  const [off, setOff] = useState(false) // a 404 has said so: nothing to keep asking for
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])

  const notice = useCallback((n: Banner) => setNotices(list => [...list.filter(x => x.id !== n.id), n]), [])
  const dismiss = useCallback((id: string) => setNotices(list => list.filter(x => x.id !== id)), [])

  const load = useCallback(() => {
    billingApi
      .get()
      .then(async b => {
        if (b === null) {
          setOff(true)
          return setBilling(null)
        }
        // Before the account is shown: the gate depends on whether there are sessions.
        const [list, cat] = await Promise.all([
          api.listSessions().catch(e => (signedOutHandled(e), null)),
          catalogueRef.current ?? billingApi.catalogue().catch(() => null),
        ])
        catalogueRef.current = cat
        setCatalogue(cat)
        if (list) setSessions(list)
        setRefused(null)
        setBilling(b)
      })
      .catch(e => {
        if (signedOutHandled(e)) return
        if (e instanceof ApiError && e.status === 403 && (e.code === "account_blocked" || e.code === "signups_paused")) {
          setRefused(e.code)
          return setBilling(null)
        }
        // Billing that cannot be read is not shown; the server still enforces.
        // What was last known stays on screen.
        setBilling(current => (current === undefined ? null : current))
      })
  }, [])

  usePolling(load, !off, 15_000)

  const leave = useCallback(
    async (action: BillingAction, open: () => Promise<{ url: string }>) => {
      setBusy(action)
      dismiss("stripe")
      try {
        const { url } = await open()
        if (billing) remember(billing.balanceMicros)
        browser.go(url)
      } catch (e) {
        if (signedOutHandled(e)) return
        const down = e instanceof ApiError && e.status === 502
        notice({
          id: "stripe",
          type: "error",
          text: down ? STRIPE_DOWN : `The payment page could not be opened: ${e instanceof Error ? e.message : e}`,
          actions: [],
          dismissible: true,
        })
      } finally {
        setBusy(null)
      }
    },
    [billing, dismiss, notice],
  )

  const run = useCallback(
    (action: BillingAction) => {
      if (action === "add-credit") return setAdding(true)
      if (action === "plans") return navigate("/billing#plan")
      if (action === "create") {
        dismiss("checkout")
        return navigate("/sessions/create")
      }
      if (action === "add-card") return void leave(action, () => billingApi.checkout())
      return void leave(action, () => billingApi.portal())
    },
    [leave, navigate, dismiss],
  )

  const subscribe = useCallback((lookupKey: string) => void leave("plans", () => billingApi.checkout(lookupKey)), [leave])

  // A subscriber changes plan here: the portal cannot (docs/billing-stripe.md).
  const changePlan = useCallback(
    async (lookupKey: string, name: string) => {
      setBusy("plans")
      dismiss("stripe")
      try {
        const done = await billingApi.changePlan(lookupKey)
        notice({ id: "stripe", type: "success", text: planChangeMessage(done, name), actions: [], dismissible: true })
        load()
      } catch (e) {
        if (signedOutHandled(e)) return
        const down = e instanceof ApiError && e.status === 502
        notice({
          id: "stripe",
          type: "error",
          text: down ? STRIPE_DOWN : `Your plan was not changed: ${e instanceof Error ? e.message : e}`,
          actions: e instanceof ApiError && e.status === 402 ? [{ action: "portal", label: "Update card" }] : [],
          dismissible: true,
        })
      } finally {
        setBusy(null)
      }
    },
    [dismiss, notice, load],
  )

  // Back from Checkout: /billing?checkout=<id>.
  const checkoutId = location.pathname === "/billing" ? new URLSearchParams(location.search).get("checkout") : null
  useEffect(() => {
    if (!checkoutId) return
    const cancelled = () => !alive.current
    // The id is used once: a reload does not confirm again.
    navigate("/billing", { replace: true })
    if (!isCheckoutId(checkoutId)) return
    notice({ id: "checkout", type: "in-progress", text: "Confirming", actions: [], dismissible: false })
    followCheckout(billingApi, checkoutId, { before: recall(), cancelled })
      .then(outcome => {
        if (cancelled()) return
        load()
        if (outcome.result === "expired" || outcome.result === "unknown") return dismiss("checkout")
        if (outcome.result === "late") {
          return notice({
            id: "checkout",
            type: "info",
            text: "Received. Your credit will appear within a few minutes.",
            actions: [],
            dismissible: true,
          })
        }
        setBilling(outcome.billing)
        const said = checkoutMessage(outcome.state, outcome.billing, catalogueRef.current)
        notice({
          id: "checkout",
          type: said.type,
          text: said.text,
          actions:
            said.action === "add-credit"
              ? [{ action: "add-credit", label: "Add credit" }]
              : said.action === "create"
                ? [{ action: "create", label: "Create your first desktop" }]
                : [],
          dismissible: true,
        })
      })
      .catch(e => {
        if (!signedOutHandled(e)) dismiss("checkout")
      })
    // Only a new id starts a new follow.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [checkoutId])

  const value = useMemo<BillingContext>(
    () => ({ billing, catalogue, sessions, refused, reload: load, busy, run, subscribe, changePlan, notices, dismiss }),
    [billing, catalogue, sessions, refused, load, busy, run, subscribe, changePlan, notices, dismiss],
  )

  return (
    <Context.Provider value={value}>
      {children}
      {adding && billing && (
        <AddCreditModal
          catalogue={catalogue}
          testMode={billing.payments === "test"}
          busy={busy === "add-credit"}
          onDismiss={() => setAdding(false)}
          onBuy={lookupKey =>
            leave("add-credit", () => billingApi.checkout(lookupKey)).then(() => setAdding(false))
          }
        />
      )}
    </Context.Provider>
  )
}
