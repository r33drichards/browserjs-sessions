# UI contract

The states the app must be able to show, what each is made of in
Cloudscape, and the copy. Wireframes are in section 6 of the design. All of
it is absent while `GET /api/billing` answers 404 (`BILLING` off).

## Where things are

| Place | What |
|---|---|
| First run (`/welcome`), shown in place of every page while `state` is `terms` or `no_card` and the user has no session | the gate |
| Top navigation | a utility showing the balance (`$7.40`), linking to `/billing`; red at zero, and "Add a card" when there is none |
| `/billing` (new page, in the side navigation as "Billing") | everything below |
| Every page, above the content (`AppLayout` `notifications`, a `Flashbar`) | the banners |
| `/sessions/create`, the sessions list, the session page | the blocked states |
| `computeruse.site/pricing` (public site) | the pricing table, from the same numbers |

Money is shown in dollars and cents, rounded down. Estimates of time ("about
37 hours") are labelled as estimates.

## The gate (`/welcome`)

A `Wizard`-like single page, `ContentLayout` with two `Container`s:

1. **Terms** (only while `termsRequired`): links to the terms, privacy and
   acceptable-use pages, one checkbox, "Continue".
2. **Add a payment method.** Text: "A card is required before you create a
   desktop. Nothing is charged now; your bank may show a temporary
   authorisation. When your card is saved you get **$5 of credit**, enough
   for about 25 hours awake, valid for 90 days. One credit per person and
   per card." Primary button "Add a card" (`POST /api/billing/checkout` with
   no item, then the browser goes to Stripe). Beneath it: what things cost
   (the two rates), and links "See plans" and "Pricing".

A user with `no_card` who already has sessions (their card was removed)
gets the normal app with the `no_card` banner instead, so that they can see,
stop and delete what they have.

## `/billing`

`ContentLayout` with a `Header` ("Billing"; actions "Add credit", "Manage
billing"), then four `Container`s.

1. **Credit.** A large figure: the balance. `KeyValuePairs`: "Using now"
   (`burnMicrosPerHour` as dollars an hour, and "about N hours left at this
   rate"), and one line per entry of `balances` in the order it will be
   used ("Plan credit $6.10, until 5 Nov", "Sign-up credit $1.30, until
   30 Dec", "Purchased credit $0.00"). For a subscriber a `ProgressBar` of
   the period's charges over `planCreditMicros` ("Plan credit used this
   period", "Resets <period.end>"). One line of text: "Credit is used at
   $0.20 for each hour a session is awake, and $1.40 a month for each
   session you keep (its 5 GB disk), awake or asleep. An idle session sleeps
   after 15 minutes; stop it to stop the hourly charge at once; delete it
   to stop the disk charge."
2. **Plan.** A pricing table (Cloudscape `Cards`, one per option, the
   current one marked): Pay as you go, Starter, Pro (Scale when enabled),
   each with price, credit a month, what that is in awake hours (an
   estimate), sessions, awake at once, and one button: "Subscribe",
   "Current plan", or for a subscriber "Change or cancel plan" (the portal).
   For a subscriber also "Renews <date>" or "Ends <date>" or "Payment
   failed" (`StatusIndicator` error).
3. **Payment method.** Brand, last four digits, expiry of the default card;
   "Manage cards" (the portal). Without one: `StatusIndicator` error "No
   payment method" and the button "Add a card". Then **Auto-recharge**
   (only when `autoRecharge.available`): a `Toggle`, and when on a `Select`
   of the pack, an `Input` for "when my balance falls below" and one for
   "at most per month"; the text of agreement below; "Charged automatically
   this month: $20 of $50".
4. **Usage.** A `Select` of periods; a stacked bar chart by day (awake
   charges, disk charges, in dollars); a `Table` of sessions (name, hours
   awake, awake charge, disk charge, total), sorted by total.

"Add credit" opens a `Modal` with the packs as `Tiles` and one primary
button "Continue to payment"; text beneath: "Credit is used after your
plan's credit and is valid for 12 months." While `payments` is `test`: an
`Alert` type info, "Test mode: no real money is taken. Use card 4242 4242
4242 4242."

Auto-recharge agreement, shown with a checkbox before it can be turned on,
and recorded: "When my balance falls below $<threshold>, charge my saved
card $<pack> for $<pack> of credit, as often as needed but not more than
$<cap> in a calendar month, until I turn this off here. Each charge appears
in my invoices." (Wording for the product owner to review.)

"Danger zone" `ExpandableSection` at the foot: "Delete account", a `Modal`
that asks for the email address to be typed.

## States

| State | Condition | Shown |
|---|---|---|
| Loading | first fetch | `Spinner` in each container |
| No billing | 404 | no navigation item, no page (the route redirects to `/`) |
| Terms | `state: terms` | the gate, step 1 |
| No card, new user | `state: no_card`, no sessions | the gate, step 2 |
| No card, has sessions | `state: no_card`, sessions | Flashbar error, not dismissible: "You have no payment method. Your sessions are asleep and kept. Add a card to wake them or create new ones." Action "Add a card". (With a zero balance and deletion on, also the date.) |
| Card saved, credit granted | returning from a setup Checkout, `signupCredit.state: granted` | Flashbar success: "Card saved. $5 of credit added, valid until <date>." Button "Create your first desktop" |
| Card saved, no credit | `signupCredit.state: refused` | Flashbar info: "Card saved." and by reason: `card-used` "This card has already been used for a sign-up credit."; `prepaid` "Prepaid cards do not get the sign-up credit."; `wallet` "Cards added through Apple Pay or Google Pay do not get the sign-up credit." Action "Add credit" |
| Ledger pending | `ledger: pending` | credit container: `StatusIndicator` pending, "Setting up your account" |
| Ledger stale | `ledger: stale` | `Alert` warning on `/billing`: "Billing is not being updated right now. The numbers are from <time>." |
| Shadow mode | `mode: meter` | `Alert` info on `/billing`: "Usage is shown for information. Nothing is limited yet." No banners, no gate. |
| OK | `level: ok` | nothing extra |
| Low | `level: low` | Flashbar warning, dismissible, once a day: "$0.80 of credit left, about 3 hours at your current use." Actions "Add credit", "See plans". With auto-recharge on: no banner. |
| Out of credit, grace | `level: exhausted`, `sleepAt` in the future, a session awake | Flashbar error, not dismissible: "You are out of credit. Running sessions go to sleep at <time>; work in progress finishes first and nothing is lost." Action "Add credit" |
| Out of credit | `level: exhausted` otherwise | Flashbar error: "You are out of credit. Your sessions are asleep and kept." plus, for a subscriber, "Your plan's credit returns on <period.end>." plus, with `deleteAt`, "They will be deleted on <date> unless you add credit." Actions "Add credit", "See plans" |
| Deletion near | `deleteAt` within 7 days / within 1 day | the same banner, not dismissible / "Your sessions will be deleted tomorrow." |
| Payment failed | `subscription.status: past_due` or `unpaid` | Flashbar error: "Your last payment failed. Update your card to keep your plan." Action "Update card" (the portal) |
| Auto-recharge failed | `autoRecharge.disabledReason` | Flashbar error: by reason: "Your card was declined, so auto-recharge is off." / "Your bank asked for confirmation, so auto-recharge is off. Add credit now to confirm with your bank." / "Auto-recharge reached your monthly cap of $50." Action "Add credit" |
| Plan ending | `cancelsAt` set | on `/billing` only: "Your plan ends on <date>. After that you pay as you go from your credit." Action "Keep plan" (the portal) |
| Blocked | `state: blocked` | the whole app is one `Alert` error with the support address |
| Returning from Checkout | `/billing?checkout=<id>` | Flashbar in-progress "Confirming"; poll `GET /api/billing/checkout/{id}` every 2 s for up to 30 s, then `GET /api/billing` until the balance has grown (up to 90 s); then the success banner for its kind ("Card saved...", "$20 of credit added", "You are on Starter. $10 of credit added."). After the time is up: info "Received. Your credit will appear within a few minutes." |
| Checkout abandoned | `status: expired`, or back with no `checkout` | nothing |
| Stripe down | 502 on checkout or portal | Flashbar error: "The payment page could not be opened. Nothing was charged. Try again." |

## Blocked create (`/sessions/create`)

The form is shown; above it an `Alert`, and the "Create session" button is
disabled, by reason:

| Reason | Alert |
|---|---|
| `payment_method_required` | error: "Add a payment method to create a session." Button "Add a card" |
| `out_of_credit` | error: "You are out of credit." Buttons "Add credit", "See plans" |
| `session_limit` | warning: "Your plan allows N sessions. Delete one, or change plan." |
| `awake_limit` | warning: "Your plan runs N sessions at once. Stop one, or change plan." |

The reason is worked out from `GET /api/billing` and the list before the
user submits; the server's refusal (the `Error`) is shown the same way if
it still comes. `at_capacity` and `metering_unavailable` come only from the
server: `Alert` info, the button stays enabled. Beneath the form, always:
"This session will use $0.20 an hour while awake and $1.40 a month while
it exists."

## Blocked wake (the session page and the list)

| `stoppedBy` | In place of the screen | In the list |
|---|---|---|
| `credit` | `StatusIndicator` stopped "Asleep: out of credit". "This session is kept as it was. It can wake once you have credit." Buttons "Add credit", "See plans" | "Asleep: out of credit"; "Resume" disabled with that tooltip |
| `payment-method` | "Asleep: no payment method". "This session is kept as it was. It can wake once you add a card." Button "Add a card" | "Asleep: no payment method" |
| `blocked` | "Suspended." | "Suspended" |

A session with `draining` shows a warning over the screen: "Finishing work
in progress, then going to sleep." Once there is credit and a card, a
session asleep for these reasons behaves as any sleeping session: it wakes
on use or on Resume; it is not woken for the user. A session with
`deleteAfter` shows "Deleted on <date> unless you add credit" in the list
and on its page.

## Public pricing (`site/pricing.md`)

Built from the catalogue at build time (the site is static):

- a table with the four options as columns (Pay as you go, Starter, Pro,
  Scale when enabled): price a month; credit a month; "about N awake hours"
  (credit less one kept session, at the awake rate; an estimate); sessions;
  awake at once;
- the two rates, in words: "$0.20 for each hour a session is awake; $1.40 a
  month for each session you keep, awake or asleep";
- the credit packs;
- the sign-up credit and that a card is required;
- these lines: a plan's credit does not carry over to the next month;
  purchased credit lasts 12 months; credit is used in the order plan,
  sign-up, purchased; cancel any time, the plan runs to the end of the paid
  month; no refunds for part months or unused credit (see the refund
  policy); when credit runs out sessions sleep and are kept; prices in US
  dollars, taxes not included.
