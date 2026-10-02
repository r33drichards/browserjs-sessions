# Billing in the web app (track E)

What the app shows of metering and billing. The states and the copy are the
contract's ([`contracts/billing/ui-states.md`](contracts/billing/ui-states.md));
the API is [`contracts/billing/backend-api.yaml`](contracts/billing/backend-api.yaml).
Every price, rate and limit on screen comes from `GET /api/billing` and
`GET /api/billing/catalogue`: the UI knows no number.

## Off is today's app

The app asks `GET /api/billing` once when it loads. A 404 (the backend has
`BILLING` off) ends it there: no balance in the header, no banners, no
`/billing` page (the route goes to `/`), no gate, and the session pages are
as they were. Nothing is asked again until the page is reloaded. An answer
that is an error other than 404 is treated the same way for drawing (the
server still enforces), and asked again every 15 s.

## Where things are

| Place | File |
|---|---|
| The client, the formatting of money and time, and the decisions (gate, blocked create, blocked wake, banners, following a Checkout) as plain functions | `web/src/billingApi.ts` |
| The account for every page, leaving for Stripe, the checkout return | `web/src/billing/BillingProvider.tsx` |
| What stands in place of the pages (gate, blocked account) | `web/src/billing/BillingGate.tsx`, `web/src/pages/Welcome.tsx` |
| The balance in the header, the banners | `web/src/billing/BillingChrome.tsx` |
| `/billing` | `web/src/pages/Billing.tsx`, `billing/PlanList.tsx`, `billing/UsageChart.tsx`, `billing/AddCreditModal.tsx` |
| The create page's alert and cost line, the session's "asleep because" | `web/src/billing/SessionBilling.tsx` |
| The mock of the API | `web/mock/billing.ts` |

The app has no side navigation and no `AppLayout` around most pages, so the
contract's "side navigation item" is the header link ("Billing $7.40", red at
zero, "Add a card" without one), and the banners are a `Flashbar` above the
page's content, under the breadcrumbs.

## Trying it

```sh
cd web && npm run dev:mock        # billing on, a subscriber with credit
MOCK_BILLING=low npm run dev:mock # start in another state
MOCK=off npm run dev:mock         # billing (and policies, tokens) off
```

While it runs, open `/api/_mock/billing/<scenario>` to change state, then
reload the app. Scenarios join with `+` (`payg+auto`, `no-card+stripe-down`):

`active` `payg` `new` `terms` `no-card` `low` `grace` `exhausted` `deleting`
`tomorrow` `past-due` `recharge-failed` `ending` `pending` `stale` `meter`
`blocked` `exempt` `session-limit` `awake-limit` `auto` `auto-on` `live`
`stripe-down` `card-used` `prepaid` `wallet` `off`

The mock has no Stripe: a Checkout's "payment page" is the app's own return
address, so "Add a card", "Add credit" and "Subscribe" complete by themselves
and show the return flow.

## Screens

From the mock, in headless Chrome: [`billing-ui/`](billing-ui/).

| Screen | Scenario |
|---|---|
| [The gate](billing-ui/01-gate.png), [with the terms](billing-ui/02-gate-terms.png) | `new`, `terms` |
| [`/billing`, a subscriber](billing-ui/03-billing.png) | `active` |
| [`/billing`, pay as you go, auto-recharge opened](billing-ui/04-billing-payg-auto-recharge.png) | `payg+auto` |
| [Add credit](billing-ui/05-add-credit.png) | `active` |
| [Back from Checkout: card saved, credit granted](billing-ui/06-checkout-return-card.png) | `new`, then "Add a card" |
| [Low balance](billing-ui/07-banner-low.png) | `low` |
| [Out of credit, grace](billing-ui/08-banner-grace.png), [the session finishing its work](billing-ui/17-session-draining.png) | `grace` |
| [Out of credit, deletion dated, in the list](billing-ui/09-list-out-of-credit.png) | `deleting` |
| [No payment method, in the list](billing-ui/10-list-no-card.png) | `no-card` |
| [Payment failed and auto-recharge failed](billing-ui/11-banner-payment-failed.png) | `past-due+recharge-failed` |
| Blocked create: [no card](billing-ui/12-create-no-card.png), [no credit](billing-ui/13-create-out-of-credit.png), [the plan's sessions](billing-ui/14-create-session-limit.png) | `no-card`, `exhausted`, `session-limit` |
| Blocked wake: [no credit](billing-ui/15-session-out-of-credit.png), [no card](billing-ui/16-session-no-card.png) | `exhausted`, `no-card` |
| [Stale ledger, plan ending](billing-ui/18-billing-stale-ending.png) | `stale+ending` |
| [Shadow mode](billing-ui/19-billing-shadow-mode.png) | `meter` |
| [Blocked account](billing-ui/20-blocked.png) | `blocked` |
| Billing off: [list](billing-ui/21-billing-off-list.png), [create](billing-ui/22-billing-off-create.png) | `off` |
| [Stripe down](billing-ui/23-stripe-down.png) | `no-card+stripe-down` |
| [Delete account](billing-ui/24-delete-account.png) | `active` |

## Where the UI goes beyond, or short of, the contract

- **Estimates of hours on a plan** use the pricing page's rule (credit less
  one kept session, at the awake rate), so Starter reads "about 42 hours",
  not the design sketch's "~40".
- **"An idle session sleeps after 15 minutes"** is written "goes to sleep by
  itself": the idle timeout is a deployment setting the API does not serve.
- **Auto-recharge has no defaults in the form**: `Catalogue` does not carry
  `autoRecharge.default*`, so the pack, threshold and cap start empty unless
  the account already has them.
- **The stale ledger's "numbers are from <time>"** needs a time the `Billing`
  schema does not have; the UI shows `observedAt` if the backend sends it and
  "the last known" otherwise.
- **The legal pages, pricing and the support address** are on the public
  site: `https://computeruse.site` and `browserjs06@gmail.com` unless
  `/config.js` sets `siteUrl` and `supportEmail`.
- **A session asleep for billing** is held back in the UI only while the
  account still lacks the credit or the card; after that Wake and Resume are
  ordinary, as the contract says.
- **Low balance** is dismissed for the day in `localStorage`; the other
  dismissible banners come back on a reload.
- **The balance before a Checkout** is kept in `sessionStorage` across the
  trip to Stripe, so that "until the balance has grown" still works when the
  credit landed before the browser came back.
