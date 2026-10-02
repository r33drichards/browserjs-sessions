# UI contract

The states the app must be able to show, what each is made of in
Cloudscape, and the copy. Wireframes are in section 6 of the design. All of
it is absent while `GET /api/billing` answers 404 (`BILLING` off).

## Where things are

| Place | What |
|---|---|
| Top navigation | a utility "Usage" showing hours left (`12 h 30 min left`), linking to `/billing`; red when `exhausted` |
| `/billing` (new page, in the side navigation as "Usage and plan") | everything below |
| Every page, above the content (`AppLayout` `notifications`, a `Flashbar`) | the banners |
| `/sessions/create`, the sessions list, the session page | the blocked states |
| `computeruse.site/pricing` (public site) | the pricing section, from the same numbers |

## `/billing`

`ContentLayout` with a `Header` ("Usage and plan"; actions "Buy hours",
"Manage billing"), then three `Container`s.

1. **Hours.** `ProgressBar` of `period.usedSeconds` over
   `period.allowanceSeconds` (label "Hours used this period", description
   "Resets <period.end>", `additionalInfo` "<left> left of <allowance>").
   Status `error` when `exhausted`. Beneath it `KeyValuePairs`: "Hours
   left" (balance), "Purchased hours" with their first expiry (only if
   any), "Sessions" (n of maxSessions), "Awake at once" (maxAwake). One
   line of text: "Hours count while a session is awake. A sleeping or
   stopped session uses none. An idle session sleeps after 15 minutes; stop
   it to stop the clock at once."
2. **Plan.** `KeyValuePairs`: plan name and price, "Renews <date>" or
   "Ends <date>" (cancelled) or "Payment failed" (`StatusIndicator` error).
   Free and pay-as-you-go: `Cards` of the plans (name, price, hours,
   sessions; one button each, "Subscribe"). Subscriber: a button "Change or
   cancel plan" (the portal).
3. **Usage.** A `Select` of periods; a bar chart of hours by day
   (Cloudscape `BarChart`, one series, the y axis in hours); a `Table` of
   sessions (name, hours, share), sorted by hours.

"Buy hours" opens a `Modal` with the packs as `Tiles` and one primary
button "Continue to payment"; text beneath: "Purchased hours are used after
your plan's hours and are valid for 12 months." While `payments` is `test`:
an `Alert` type info, "Test mode: no real money is taken. Use card 4242
4242 4242 4242."

"Danger zone" `ExpandableSection` at the foot: "Delete account", a `Modal`
that asks for the email address to be typed.

## States

| State | Condition | Shown |
|---|---|---|
| Loading | first fetch | `Spinner` in each container |
| No billing | 404 | no navigation item, no page (the route redirects to `/`) |
| Ledger pending | `ledger: pending` | hours container: `StatusIndicator` pending, "Setting up your account" |
| Ledger stale | `ledger: stale` | `Alert` warning on `/billing`: "Usage is not being updated right now. The numbers are from <time>." |
| Shadow mode | `mode: meter` | `Alert` info on `/billing`: "Usage is shown for information. Nothing is limited yet." No banners. |
| OK | `level: ok` | nothing extra |
| Low | `level: low` | Flashbar warning, dismissible, once per period: "<left> of your hours left this period." Actions "Buy hours", "See plans" (free) |
| Out of hours, grace | `level: exhausted` and `sleepAt` in the future, a session awake | Flashbar error, not dismissible: "You are out of hours. Running sessions go to sleep at <time>; nothing is lost." Action "Buy hours" |
| Out of hours | `level: exhausted` otherwise | Flashbar error: "You are out of hours. Your sessions are asleep and kept. Hours return on <period.end>, or buy more now." Actions "Buy hours", "See plans" |
| Payment failed | `subscription.status: past_due` or `unpaid` | Flashbar error: "Your last payment failed. Update your card to keep your plan." Action "Update card" (the portal) |
| Plan ending | `cancelsAt` set | on `/billing` only: "Your plan ends on <date>. After that you have the free allowance." Action "Keep plan" (the portal) |
| Blocked | 403 `account_blocked` | the whole app is one `Alert` error with the support address |
| Terms | `termsRequired` | a `Modal` that cannot be dismissed, links to the terms, privacy and acceptable-use pages, one checkbox, "Continue" |
| Returning from Checkout | `/billing?checkout=<id>` | Flashbar in-progress "Confirming your payment"; poll `GET /api/billing/checkout/{id}` every 2 s for up to 30 s, then `GET /api/billing` until the balance has grown (up to 90 s); then success "20 hours added" or "You are on Starter". After the time is up: info "Your payment was received. The hours will appear within a few minutes." |
| Checkout abandoned | `status: expired`, or back with no `checkout` | nothing |
| Stripe down | 502 on checkout or portal | Flashbar error: "The payment page could not be opened. Nothing was charged. Try again." |

## Blocked create (`/sessions/create`)

The form is shown; above it an `Alert` and the "Create session" button is
disabled, by reason:

| Reason | Alert |
|---|---|
| `out_of_hours` | error: "You are out of hours." Buttons "Buy hours", "See plans" |
| `session_limit` | warning: "Your plan allows N sessions. Delete one, or change plan." |
| `awake_limit` | warning: "Your plan runs N sessions at once. Stop one, or change plan." |

The reason is worked out from `GET /api/billing` and the list before the
user submits; the server's refusal (the `Error`) is shown the same way if
it still comes. `at_capacity` and `metering_unavailable` come only from the
server: `Alert` info, "Every desktop is in use right now. Try again in a
few minutes.", the button stays enabled.

## Blocked wake (the session page)

A session with `stoppedBy: billing` shows, in place of the screen, a
`Box` with `StatusIndicator` stopped "Asleep: out of hours", the text
"This session is kept as it was. It wakes when you have hours.", and the
buttons "Buy hours" and "See plans". "Resume" in the list is disabled with
the same reason as its tooltip. Once there is a balance the page behaves
as for any sleeping session. A session with `deleteAfter` shows "Deleted on
<date> unless used" in the list (a `StatusIndicator` warning) and on its
page.

## Public pricing section (`site/pricing.md`)

A table from `GET /api/billing/catalogue`'s numbers, copied at build time
(the site is static): Free, Starter, Pro (and Scale when enabled) as
columns with price, hours a month, sessions, awake at once; a second small
table of the packs; and these lines: what counts as an hour; unused plan
hours do not carry over; purchased hours last 12 months; cancel any time,
the plan runs to the end of the paid month; no refunds for part months
(see the refund policy); prices in US dollars, taxes not included.
