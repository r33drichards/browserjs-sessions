// /billing: the credit and what is using it, the plan, the payment method
// with auto-recharge, and the usage of a period. Docs: docs/billing-ui.md.
import Alert from "@cloudscape-design/components/alert"
import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import Checkbox from "@cloudscape-design/components/checkbox"
import Container from "@cloudscape-design/components/container"
import ExpandableSection from "@cloudscape-design/components/expandable-section"
import FormField from "@cloudscape-design/components/form-field"
import Header from "@cloudscape-design/components/header"
import Input from "@cloudscape-design/components/input"
import KeyValuePairs from "@cloudscape-design/components/key-value-pairs"
import Modal from "@cloudscape-design/components/modal"
import ProgressBar from "@cloudscape-design/components/progress-bar"
import Select from "@cloudscape-design/components/select"
import SpaceBetween from "@cloudscape-design/components/space-between"
import Spinner from "@cloudscape-design/components/spinner"
import StatusIndicator from "@cloudscape-design/components/status-indicator"
import Table from "@cloudscape-design/components/table"
import Toggle from "@cloudscape-design/components/toggle"
import { useEffect, useState } from "react"
import { Link, Navigate, useLocation } from "react-router-dom"
import type { Session } from "../api"
import { useMe } from "../auth/MeProvider"
import { signedOutHandled } from "../auth/signedOut"
import { useBilling } from "../billing/BillingProvider"
import { PlanList } from "../billing/PlanList"
import { UsageChart } from "../billing/UsageChart"
import type { AutoRecharge, Billing as Account, Catalogue, Usage } from "../billingApi"
import {
  SOURCE_LABEL,
  aboutHours,
  billingApi,
  browser,
  clock,
  credit,
  dollars,
  drawing,
  duration,
  enforced,
  hoursLeft,
  longDate,
  price,
  ratesInWords,
  shortDate,
} from "../billingApi"
import { Shell } from "../shell"

const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`
const message = (e: unknown) => String(e instanceof Error ? e.message : e)

export function Billing() {
  const { billing, catalogue, sessions, run, busy } = useBilling()
  const location = useLocation()

  // "See plans" lands on the plan picker.
  useEffect(() => {
    if (billing && location.hash === "#plan") document.getElementById("plan")?.scrollIntoView?.()
  }, [billing === undefined, location.hash, location.key]) // eslint-disable-line react-hooks/exhaustive-deps

  if (billing === null) return <Navigate to="/" replace />
  const crumbs = [{ text: "Billing", href: "/billing" }]
  if (billing === undefined) {
    return (
      <Shell breadcrumbs={crumbs}>
        <Spinner /> Loading billing
      </Shell>
    )
  }

  const canPay = billing.payments !== "off"
  const sub = billing.subscription

  return (
    <Shell breadcrumbs={crumbs}>
      <SpaceBetween size="l">
        <Header
          variant="h1"
          actions={
            canPay && (
              <SpaceBetween direction="horizontal" size="xs">
                <Button variant="primary" onClick={() => run("add-credit")}>
                  Add credit
                </Button>
                {billing.hasCustomer && (
                  <Button loading={busy === "portal"} onClick={() => run("portal")}>
                    Manage billing
                  </Button>
                )}
              </SpaceBetween>
            )
          }
        >
          Billing
        </Header>

        {billing.mode === "meter" && <Alert type="info">Usage is shown for information. Nothing is limited yet.</Alert>}
        {billing.state === "exempt" && billing.mode === "enforce" && (
          <Alert type="info">This account is exempt from billing. Usage is shown for the record.</Alert>
        )}
        {billing.ledger === "stale" && (
          <Alert type="warning">
            Billing is not being updated right now.
            {billing.observedAt ? ` The numbers are from ${clock(billing.observedAt)}.` : " The numbers are the last known."}
          </Alert>
        )}
        {sub?.cancelsAt && (
          <Alert type="info" action={<Button onClick={() => run("portal")}>Keep plan</Button>}>
            Your plan ends on {longDate(sub.cancelsAt)}. After that you pay as you go from your credit.
          </Alert>
        )}

        <Container header={<Header variant="h2">Credit</Header>}>
          <CreditBox billing={billing} sessions={sessions} />
        </Container>

        <div id="plan">
          <Container header={<Header variant="h2">Plan</Header>}>
            <SpaceBetween size="m">
              <PlanList choose />
              {sub && (sub.status === "past_due" || sub.status === "unpaid") ? (
                <StatusIndicator type="error">Payment failed</StatusIndicator>
              ) : sub?.cancelsAt ? (
                <Box>Ends {longDate(sub.cancelsAt)}</Box>
              ) : sub?.renewsAt ? (
                <Box>Renews {longDate(sub.renewsAt)}</Box>
              ) : null}
            </SpaceBetween>
          </Container>
        </div>

        <Container header={<Header variant="h2">Payment method</Header>}>
          <SpaceBetween size="m">
            <div className="wf-row">
              {billing.hasPaymentMethod ? (
                <Box>{cardLine(billing.card)}</Box>
              ) : (
                <StatusIndicator type={enforced(billing) ? "error" : "info"}>No payment method</StatusIndicator>
              )}
              {!canPay ? (
                <span className="wf-note">Payments are not set up yet.</span>
              ) : billing.hasPaymentMethod ? (
                <Button loading={busy === "portal"} onClick={() => run("portal")}>
                  Manage cards
                </Button>
              ) : (
                <Button loading={busy === "add-card"} onClick={() => run("add-card")}>
                  Add a card
                </Button>
              )}
            </div>
            {billing.autoRecharge?.available && (
              <AutoRechargeBox key={JSON.stringify(billing.autoRecharge)} setting={billing.autoRecharge} catalogue={catalogue} />
            )}
          </SpaceBetween>
        </Container>

        <UsageBox period={billing.period} />

        <ExpandableSection headerText="Danger zone" variant="container">
          <DeleteAccount />
        </ExpandableSection>
      </SpaceBetween>
    </Shell>
  )
}

function cardLine(card: Account["card"]): string {
  if (!card?.last4) return "A card is saved"
  const brand = card.brand ? card.brand[0].toUpperCase() + card.brand.slice(1) : "Card"
  const expires =
    card.expMonth && card.expYear ? `, expires ${String(card.expMonth).padStart(2, "0")}/${String(card.expYear).slice(-2)}` : ""
  return `${brand} ···· ${card.last4}${expires}`
}

function CreditBox({ billing, sessions }: { billing: Account; sessions: Session[] }) {
  if (billing.ledger === "pending") return <StatusIndicator type="pending">Setting up your account</StatusIndicator>

  const now = drawing(billing, sessions)
  const hours = hoursLeft(billing.balanceMicros, billing.burnMicrosPerHour)
  const words = ratesInWords(billing.rates)
  const planCredit = billing.period.planCreditMicros
  const charged = billing.period.awakeMicros + billing.period.diskMicros

  return (
    <SpaceBetween size="m">
      <div className="wf-figure" data-testid="balance" data-zero={billing.balanceMicros <= 0 || undefined}>
        {dollars(billing.balanceMicros)}
      </div>
      <KeyValuePairs
        columns={2}
        items={[
          {
            label: "Using now",
            value:
              now.perHour > 0 ? (
                <>
                  {dollars(now.perHour)} an hour{hours === null ? "" : ` · ${aboutHours(hours)} left at this rate (an estimate)`}
                  <div className="wf-note">
                    {plural(now.awake, "session", "sessions")} awake, {plural(now.kept, "session", "sessions")} kept
                  </div>
                </>
              ) : (
                "Nothing is using credit now"
              ),
          },
          ...(billing.balances ?? []).map(x => ({
            label: SOURCE_LABEL[x.source] ?? "Credit",
            value: `${dollars(x.micros)}${x.expiresAt && x.micros > 0 ? `, until ${shortDate(x.expiresAt)}` : ""}`,
          })),
        ]}
      />
      {planCredit ? (
        <ProgressBar
          label="Plan credit used this period"
          value={Math.min((charged / planCredit) * 100, 100)}
          additionalInfo={`${dollars(Math.min(charged, planCredit))} of ${dollars(planCredit)}`}
          description={`Resets ${longDate(billing.period.end)}`}
        />
      ) : null}
      <Box color="text-body-secondary">
        Credit is used at {words.awake} for each hour a session is awake, and {words.kept} a month for each session you
        keep (its {billing.rates.sessionDiskGB} GB disk), awake or asleep. An idle session goes to sleep by itself; stop it
        to stop the hourly charge at once; delete it to stop the disk charge.
      </Box>
    </SpaceBetween>
  )
}

const toDollars = (text: string) => (/^\d+(\.\d{1,2})?$/.test(text.trim()) ? Number(text) : NaN)

function AutoRechargeBox({ setting, catalogue }: { setting: AutoRecharge; catalogue: Catalogue | null }) {
  const { reload } = useBilling()
  const packs = catalogue?.packs ?? []
  const [on, setOn] = useState(setting.enabled)
  const [pack, setPack] = useState(setting.pack ?? "")
  const [threshold, setThreshold] = useState(setting.thresholdMicros === undefined ? "" : String(setting.thresholdMicros / 1_000_000))
  const [cap, setCap] = useState(setting.monthlyCapCents === undefined ? "" : String(setting.monthlyCapCents / 100))
  const [agree, setAgree] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")

  const chosen = packs.find(p => p.key === pack)
  const thresholdUsd = toDollars(threshold)
  const capUsd = toDollars(cap)
  const complete = !!chosen && thresholdUsd > 0 && capUsd > 0
  const options = packs.map(p => ({ value: p.key, label: credit(p.creditMicros) }))

  async function save(enabled: boolean) {
    setBusy(true)
    setError("")
    try {
      await billingApi.setAutoRecharge(
        enabled
          ? { enabled, pack, thresholdMicros: Math.round(thresholdUsd * 1_000_000), monthlyCapCents: Math.round(capUsd * 100), agree: true }
          : { enabled },
      )
      reload()
    } catch (e) {
      if (!signedOutHandled(e)) setError(message(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <SpaceBetween size="s">
      <Toggle
        checked={on}
        disabled={busy}
        onChange={e => {
          setOn(e.detail.checked)
          // Turning it off needs no more than this; turning it on needs the form below.
          if (!e.detail.checked && setting.enabled) void save(false)
        }}
      >
        Auto-recharge
      </Toggle>
      {on && (
        <SpaceBetween size="s">
          <div className="wf-fields">
            <FormField label="Buy">
              <Select
                selectedOption={options.find(o => o.value === pack) ?? null}
                options={options}
                placeholder="Choose an amount"
                onChange={e => setPack(e.detail.selectedOption.value ?? "")}
                ariaLabel="Amount to buy"
              />
            </FormField>
            <FormField label="when my balance falls below ($)">
              <Input value={threshold} inputMode="decimal" onChange={e => setThreshold(e.detail.value)} />
            </FormField>
            <FormField label="at most per month ($)">
              <Input value={cap} inputMode="decimal" onChange={e => setCap(e.detail.value)} />
            </FormField>
          </div>
          {complete && (
            <Checkbox checked={agree} onChange={e => setAgree(e.detail.checked)}>
              When my balance falls below {price(Math.round(thresholdUsd * 100))}, charge my saved card {price(chosen.amount)} for{" "}
              {credit(chosen.creditMicros)} of credit, as often as needed but not more than {price(Math.round(capUsd * 100))} in a
              calendar month, until I turn this off here. Each charge appears in my invoices.
            </Checkbox>
          )}
          {setting.chargedCents !== undefined && setting.monthlyCapCents !== undefined && (
            <Box color="text-body-secondary">
              Charged automatically this month: {price(setting.chargedCents)} of {price(setting.monthlyCapCents)}
            </Box>
          )}
          <Box>
            <Button disabled={!complete || !agree} loading={busy} onClick={() => save(true)}>
              {setting.enabled ? "Save auto-recharge" : "Turn on auto-recharge"}
            </Button>
          </Box>
        </SpaceBetween>
      )}
      {error ? <Alert type="error">{error}</Alert> : null}
    </SpaceBetween>
  )
}

function UsageBox({ period }: { period: Account["period"] }) {
  const [chosen, setChosen] = useState("") // "" is the current period
  const [usage, setUsage] = useState<Usage | null>(null)
  const [periods, setPeriods] = useState<string[]>([])
  const [error, setError] = useState("")

  useEffect(() => {
    let cancelled = false
    setUsage(null)
    setError("")
    billingApi
      .usage(chosen || undefined)
      .then(u => {
        if (cancelled) return
        setUsage(u)
        setPeriods(u.periods ?? [])
      })
      .catch(e => {
        if (!cancelled && !signedOutHandled(e)) setError(message(e))
      })
    return () => {
      cancelled = true
    }
  }, [chosen])

  const options = [
    { value: "", label: `${shortDate(period.start)} – ${shortDate(period.end)} (current)` },
    ...periods.map(p => ({ value: p, label: `from ${longDate(p)}` })),
  ]
  const rows = [...(usage?.sessions ?? [])]
    .map(s => ({ ...s, total: (s.awakeMicros ?? 0) + (s.diskMicros ?? 0) }))
    .sort((a, b) => b.total - a.total)

  return (
    <Container
      header={
        <Header
          variant="h2"
          actions={
            <Select
              ariaLabel="Period"
              selectedOption={options.find(o => o.value === chosen) ?? options[0]}
              options={options}
              onChange={e => setChosen(e.detail.selectedOption.value ?? "")}
            />
          }
        >
          Usage
        </Header>
      }
    >
      {error ? (
        <Box>⚠ Couldn&apos;t load usage: {error}</Box>
      ) : !usage ? (
        <Spinner />
      ) : (
        <SpaceBetween size="m">
          <Box>
            {dollars(usage.awakeMicros + usage.diskMicros)} in this period: {dollars(usage.awakeMicros)} awake (
            {duration(usage.awakeSeconds)}), {dollars(usage.diskMicros)} disk
          </Box>
          <UsageChart days={usage.days} />
          <div className="wf-plain">
          <Table
            variant="embedded"
            items={rows}
            trackBy="id"
            ariaLabels={{ tableLabel: "Usage by session" }}
            columnDefinitions={[
              {
                id: "name",
                header: "Session",
                cell: s => (s.name ? <Link to={`/sessions/${s.id}`}>{s.name}</Link> : <span className="wf-note">deleted ({s.id})</span>),
              },
              { id: "awake", header: "Awake", cell: s => duration(s.awakeSeconds ?? 0) },
              { id: "awake$", header: "Awake $", cell: s => dollars(s.awakeMicros ?? 0) },
              { id: "disk$", header: "Disk $", cell: s => dollars(s.diskMicros ?? 0) },
              { id: "total", header: "Total", cell: s => dollars(s.total) },
            ]}
            empty={<Box textAlign="center">No sessions were charged in this period.</Box>}
          />
          </div>
        </SpaceBetween>
      )}
    </Container>
  )
}

function DeleteAccount() {
  const me = useMe()
  const [open, setOpen] = useState(false)
  const [typed, setTyped] = useState("")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")

  async function remove() {
    setBusy(true)
    setError("")
    try {
      await billingApi.deleteAccount(typed.trim())
      // The account is gone: sign out.
      browser.go(window.__BROWSERJS_CFG__?.signOutUrl ?? "/.pomerium/sign_out")
    } catch (e) {
      if (!signedOutHandled(e)) setError(message(e))
      setBusy(false)
    }
  }

  return (
    <>
      <div className="wf-row">
        <Box>Deletes every session with its disk, every API token, your saved cards and your remaining credit.</Box>
        <Button onClick={() => setOpen(true)}>Delete account</Button>
      </div>
      {open && (
        <Modal
          visible
          onDismiss={() => setOpen(false)}
          header="Delete account"
          footer={
            <Box float="right">
              <SpaceBetween direction="horizontal" size="xs">
                <Button variant="link" onClick={() => setOpen(false)}>
                  Cancel
                </Button>
                <Button variant="primary" disabled={typed.trim() !== me.email} loading={busy} onClick={remove}>
                  Delete account
                </Button>
              </SpaceBetween>
            </Box>
          }
        >
          <SpaceBetween size="m">
            <Box>
              This deletes every session (its disk and snapshots) and every API token, cancels your plan at once with no
              refund, removes your saved cards and gives up your remaining credit. It cannot be undone.
            </Box>
            <FormField label={`Type ${me.email} to confirm`}>
              <Input value={typed} onChange={e => setTyped(e.detail.value)} ariaLabel="Your email address" />
            </FormField>
            {error ? <Alert type="error">{error}</Alert> : null}
          </SpaceBetween>
        </Modal>
      )}
    </>
  )
}
