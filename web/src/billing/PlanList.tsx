// The options side by side, from the catalogue: pay as you go and each plan,
// with what its credit is in awake hours (an estimate).
import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import Cards from "@cloudscape-design/components/cards"
import type { Item } from "../billingApi"
import { aboutHours, awakeHours, credit, price } from "../billingApi"
import { useBilling } from "./BillingProvider"

interface Option {
  key: string
  name: string
  item?: Item // absent for pay as you go
  maxSessions?: number
  maxAwake?: number
}

const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`

// With `choose`, each card ends in its button: Subscribe, or Current plan;
// for a subscriber, Upgrade or Downgrade (done here: Stripe's portal cannot
// change a plan), and on pay as you go, Cancel plan (the portal).
export function PlanList({ choose = false }: { choose?: boolean }) {
  const { billing, catalogue, run, subscribe, changePlan, busy } = useBilling()
  if (!billing || !catalogue) return null
  const options: Option[] = [
    { key: "payg", name: "Pay as you go", ...catalogue.payg },
    ...catalogue.plans.map(p => ({ key: p.key, name: p.name, item: p, maxSessions: p.maxSessions, maxAwake: p.maxAwake })),
  ]
  const subscribed = billing.plan.key !== "payg"
  const canPay = billing.payments !== "off"
  const ending = !!billing.subscription?.cancelsAt
  const dearer = (o: Option) => (o.item?.amount ?? 0) > (billing.plan.amount ?? 0)

  return (
    <div className="wf-plain">
    <Cards
      ariaLabels={{ itemSelectionLabel: () => "", selectionGroupLabel: "Plans" }}
      cardsPerRow={[{ cards: 1 }, { minWidth: 500, cards: Math.min(options.length, 4) }]}
      items={options}
      trackBy="key"
      cardDefinition={{
        header: o => (
          <span data-testid={`plan-${o.key}`}>
            {o.name}
            {o.key === billing.plan.key && choose ? " ●" : ""}
          </span>
        ),
        sections: [
          { id: "price", content: o => (o.item ? `${price(o.item.amount)} a month` : "no monthly fee") },
          { id: "credit", content: o => (o.item ? `${credit(o.item.creditMicros)} of credit a month` : "credit at face value") },
          {
            id: "hours",
            content: o =>
              o.item ? `${aboutHours(awakeHours(o.item.creditMicros, billing.rates, 1))} awake with one session kept (an estimate)` : "buy credit when you need it",
          },
          { id: "sessions", content: o => (o.maxSessions === undefined ? "" : plural(o.maxSessions, "session", "sessions")) },
          { id: "awake", content: o => (o.maxAwake === undefined ? "" : `${o.maxAwake} awake at once`) },
          ...(choose
            ? [
                {
                  id: "action",
                  content: (o: Option) =>
                    o.key === billing.plan.key ? (
                      <Box fontWeight="bold">Current plan</Box>
                    ) : !canPay ? null : subscribed && !o.item ? (
                      <Button loading={busy === "portal"} onClick={() => run("portal")} ariaLabel="Cancel plan">
                        Cancel plan
                      </Button>
                    ) : subscribed && ending ? null : subscribed ? (
                      <Button
                        loading={busy === "plans"}
                        onClick={() => changePlan(o.item!.lookupKey, o.name)}
                        ariaLabel={`${dearer(o) ? "Upgrade" : "Downgrade"} to ${o.name}`}
                      >
                        {dearer(o) ? "Upgrade now" : "Downgrade at the period's end"}
                      </Button>
                    ) : o.item ? (
                      <Button loading={busy === "plans"} onClick={() => subscribe(o.item!.lookupKey)} ariaLabel={`Subscribe to ${o.name}`}>
                        Subscribe
                      </Button>
                    ) : null,
                },
              ]
            : []),
        ],
      }}
    />
    </div>
  )
}
