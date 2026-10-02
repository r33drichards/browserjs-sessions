// What stands in place of the pages: nothing while billing is first asked
// for, the first-run gate for a new user, and one alert for an account that
// is blocked. Otherwise, and always where billing is off, the pages.
import Alert from "@cloudscape-design/components/alert"
import { Suspense, lazy } from "react"
import { gateStep } from "../billingApi"
import { Shell } from "../shell"
import { useBilling } from "./BillingProvider"
import { supportEmail } from "./site"

const Welcome = lazy(() => import("../pages/Welcome").then(m => ({ default: m.Welcome })))

export function BillingGate({ children }: { children: React.ReactNode }) {
  const { billing, sessions, refused } = useBilling()
  // One request, answered before the first page: a new user never sees the
  // list flash by before the gate.
  if (billing === undefined) return null

  if (refused === "signups_paused") {
    return (
      <Shell>
        <Alert type="info" header="Sign-ups are paused for today">
          Try again tomorrow.
        </Alert>
      </Shell>
    )
  }
  if (refused === "account_blocked" || (billing?.state === "blocked" && billing.mode === "enforce")) {
    const address = supportEmail()
    return (
      <Shell>
        <Alert type="error" header="This account is suspended">
          Your sessions are asleep and nothing can be created or woken. Write to <a href={`mailto:${address}`}>{address}</a>.
        </Alert>
      </Shell>
    )
  }

  const step = billing ? gateStep(billing, sessions.length) : null
  if (step) {
    return (
      <Suspense fallback={null}>
        <Welcome step={step} />
      </Suspense>
    )
  }
  return <>{children}</>
}
