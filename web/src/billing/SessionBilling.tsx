// What billing shows on the pages about sessions: why one is asleep and
// cannot wake, when it is due for deletion, and why a new one cannot be made.
// All of it is nothing where billing is off.
import Alert from "@cloudscape-design/components/alert"
import Button from "@cloudscape-design/components/button"
import SpaceBetween from "@cloudscape-design/components/space-between"
import StatusIndicator from "@cloudscape-design/components/status-indicator"
import { useState } from "react"
import type { Session } from "../api"
import type { BillingAction, Refusal, WakeBlock } from "../billingApi"
import { WAKE_BLOCK_LABEL, awakeRate, createRefusal, dayMonth, dollars, planIncludes, ratesInWords, refusalOf, wakeBlock } from "../billingApi"
import { stateLabel } from "../components/SessionLifecycle"
import { useBilling } from "./BillingProvider"

type Blockable = Pick<Session, "state" | "stoppedBy" | "deleteAfter" | "draining" | "stateSaved">

// Why this session cannot wake now, or null.
export function useWakeBlock(session: Blockable | null): WakeBlock | null {
  const { billing } = useBilling()
  return session ? wakeBlock(session, billing) : null
}

// The state as the list and the title row show it, with the reason it is
// asleep when that reason is billing.
export function SessionState({ session }: { session: Blockable }) {
  const block = useWakeBlock(session)
  return (
    <>
      <span className="wf-state" data-state={session.state} data-blocked={block ?? undefined}>
        {block ? WAKE_BLOCK_LABEL[block] : stateLabel(session)}
      </span>
      {session.deleteAfter && <span className="wf-note wf-delete-after"> {deletedOn(session.deleteAfter)}</span>}
    </>
  )
}

const deletedOn = (at: string) => `Deleted on ${dayMonth(at)} unless you add credit`

function Actions({ actions }: { actions: { action: BillingAction; label: string }[] }) {
  const { run, busy } = useBilling()
  return (
    <SpaceBetween direction="horizontal" size="xs">
      {actions.map(a => (
        <Button key={a.action} formAction="none" loading={busy === a.action} onClick={() => run(a.action)}>
          {a.label}
        </Button>
      ))}
    </SpaceBetween>
  )
}

const ADD_CREDIT = { action: "add-credit", label: "Add credit" } as const
const SEE_PLANS = { action: "plans", label: "See plans" } as const
const ADD_CARD = { action: "add-card", label: "Add a card" } as const

// In place of the screen of a session that billing keeps asleep.
export function BlockedWake({ block, deleteAfter }: { block: WakeBlock; deleteAfter?: string }) {
  return (
    <div className="wf-placeholder" data-testid="blocked-wake">
      <SpaceBetween size="s" alignItems="center">
        <StatusIndicator type="stopped">{block === "blocked" ? "Suspended." : WAKE_BLOCK_LABEL[block]}</StatusIndicator>
        {block !== "blocked" && (
          <p>
            This session is kept as it was. It can wake once you {block === "credit" ? "have credit" : "add a card"}.
          </p>
        )}
        {deleteAfter && <p>{deletedOn(deleteAfter)}.</p>}
        {block === "credit" && <Actions actions={[ADD_CREDIT, SEE_PLANS]} />}
        {block === "payment-method" && <Actions actions={[ADD_CARD]} />}
      </SpaceBetween>
    </div>
  )
}

// Over the screen of a session that is finishing its calls before a sleep.
export function DrainingNote({ session }: { session: Blockable }) {
  if (!session.draining || session.draining === "user" || session.draining === "sleep" || session.draining === "idle") return null
  return <Alert type="warning">Finishing work in progress, then going to sleep.</Alert>
}

const DISABLING = new Set(["payment_method_required", "out_of_credit", "session_limit", "awake_limit", "account_blocked", "terms_required"])

function refusalAlert(r: Refusal) {
  switch (r.code) {
    case "payment_method_required":
      return (
        <Alert type="error" action={<Actions actions={[ADD_CARD]} />}>
          Add a payment method to create a session.
        </Alert>
      )
    case "out_of_credit":
      return (
        <Alert type="error" action={<Actions actions={[ADD_CREDIT, SEE_PLANS]} />}>
          You are out of credit.
        </Alert>
      )
    case "session_limit":
      return (
        <Alert type="warning" action={<Actions actions={[SEE_PLANS]} />}>
          {r.limit === undefined ? r.message : `Your plan allows ${r.limit} sessions. Delete one, or change plan.`}
        </Alert>
      )
    case "awake_limit":
      return (
        <Alert type="warning" action={<Actions actions={[SEE_PLANS]} />}>
          {r.limit === undefined ? r.message : `Your plan runs ${r.limit} sessions at once. Stop one, or change plan.`}
        </Alert>
      )
    case "account_blocked":
      return <Alert type="error">This account is suspended. Contact support.</Alert>
    case "terms_required":
      return <Alert type="error">Accept the terms to continue.</Alert>
    case "size_not_included":
      return (
        <Alert type="warning" action={<Actions actions={[SEE_PLANS]} />}>
          Your plan does not include sessions of this size. Pick a smaller size, or change plan.
        </Alert>
      )
    default:
      // at_capacity, metering_unavailable, rate_limited: the server's own sentence; trying again is right.
      return <Alert type="info">{r.message ?? "Try again in a few minutes."}</Alert>
  }
}

// The create page's share of billing: the alert above the form, whether the
// button is disabled, the line beneath the form, and what to do with a
// refusal that still comes from the server.
// `size` is the size chosen on the form: the cost is that size's.
export function useCreateGate(size?: string) {
  const { billing, sessions, reload } = useBilling()
  const [fromServer, setFromServer] = useState<Refusal | null>(null)
  const known = billing ? createRefusal(billing, sessions) : null
  const refusal = known ?? fromServer
  const words = billing ? { ...ratesInWords(billing.rates), awake: dollars(awakeRate(billing, size)) } : null

  return {
    alert: refusal ? <div data-testid="create-refusal">{refusalAlert(refusal)}</div> : null,
    // Only what the account itself says: a refusal by the server may have passed.
    disabled: known !== null && DISABLING.has(known.code),
    note: words ? (
      <p className="wf-note" data-testid="create-cost">
        This session will use {words.awake} an hour while awake and {words.kept} a month while it exists.
      </p>
    ) : null,
    // What an awake hour of a session of a size costs ("$0.40"), and whether
    // the plan includes that size. Null and true where billing is off.
    hourly: (of: string) => (billing ? dollars(awakeRate(billing, of)) : null),
    includes: (of: string) => !billing || billing.mode !== "enforce" || billing.state === "exempt" || planIncludes(billing, of),
    clear: () => setFromServer(null),
    // True when the error was a billing refusal, now shown as the alert.
    refused(e: unknown): boolean {
      const r = refusalOf(e)
      if (!r) return false
      setFromServer(r)
      reload()
      return true
    },
  }
}
