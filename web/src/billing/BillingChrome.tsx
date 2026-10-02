// What billing adds around every page: the balance in the header, and the
// banners above the content. Both are nothing where billing is off.
import Button from "@cloudscape-design/components/button"
import Flashbar from "@cloudscape-design/components/flashbar"
import SpaceBetween from "@cloudscape-design/components/space-between"
import { useMemo, useState } from "react"
import { Link } from "react-router-dom"
import type { Banner } from "../billingApi"
import { banners, dollars, enforced } from "../billingApi"
import { useBilling } from "./BillingProvider"

// "$7.40", linking to the billing page; marked at zero; "Add a card" without one.
export function BillingNav() {
  const { billing } = useBilling()
  // Nothing to look at yet for an account that is blocked or still at the terms.
  if (!billing || (billing.mode === "enforce" && (billing.state === "blocked" || billing.state === "terms"))) return null
  const noCard = enforced(billing) && billing.state === "no_card"
  const zero = billing.ledger !== "pending" && billing.balanceMicros <= 0
  return (
    <>
      <Link to="/billing" className={zero || noCard ? "wf-balance wf-balance-zero" : "wf-balance"} aria-label="Billing">
        {noCard ? "Add a card" : billing.ledger === "pending" ? "Billing" : `Billing ${dollars(billing.balanceMicros)}`}
      </Link>{" "}
      ·{" "}
    </>
  )
}

const LOW_KEY = "browserjs.lowBalanceDismissed"
const today = () => new Date().toISOString().slice(0, 10)

function lowDismissedToday(): boolean {
  try {
    return localStorage.getItem(LOW_KEY) === today()
  } catch {
    return false
  }
}

export function BillingBanners() {
  const { billing, sessions, notices, dismiss, run, busy } = useBilling()
  // "low" is put away for the day; the others until the page is loaded again.
  const [hidden, setHidden] = useState<string[]>(() => (lowDismissedToday() ? ["low"] : []))

  const items = useMemo(() => {
    const standing = billing ? banners(billing, sessions).filter(b => !hidden.includes(b.id)) : []
    return [...notices, ...standing]
  }, [billing, sessions, notices, hidden])

  if (items.length === 0) return null

  function onDismiss(b: Banner) {
    if (notices.some(n => n.id === b.id)) return dismiss(b.id)
    if (b.id === "low") {
      try {
        localStorage.setItem(LOW_KEY, today())
      } catch {}
    }
    setHidden(list => [...list, b.id])
  }

  return (
    <div className="wf-flash" data-testid="billing-banners">
      <Flashbar
        items={items.map(b => ({
          id: b.id,
          type: b.type === "in-progress" ? "info" : b.type,
          loading: b.type === "in-progress",
          content: b.text,
          dismissible: b.dismissible,
          dismissLabel: "Dismiss",
          onDismiss: () => onDismiss(b),
          action:
            b.actions.length > 0 ? (
              <SpaceBetween direction="horizontal" size="xs">
                {b.actions.map(a => (
                  <Button key={a.action} loading={busy === a.action} onClick={() => run(a.action)}>
                    {a.label}
                  </Button>
                ))}
              </SpaceBetween>
            ) : undefined,
        }))}
      />
    </div>
  )
}
