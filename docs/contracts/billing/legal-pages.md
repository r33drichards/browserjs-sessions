# Public pages needed before sign-up opens or live payments start

**Drafts of what each page has to say, for the product owner to review,
rewrite and have checked. This is not legal advice, and none of it has been
reviewed by a lawyer.** They exist because two reviews ask for them:
Stripe's website checklist for account activation
(https://docs.stripe.com/get-started/checklist/website) and Google's brand
verification of the OAuth app (a homepage that links a privacy policy on a
verified domain,
https://developers.google.com/identity/protocols/oauth2/production-readiness/brand-verification).

Pages go in `site/legal/` (VitePress), linked from the site's footer and
from the app's footer, each with a "last updated" date and a version that
`TERMS_VERSION` names.

| Page | Needed for |
|---|---|
| `/legal/terms` | open sign-up; Checkout's terms checkbox |
| `/legal/privacy` | Google brand verification; Stripe; open sign-up |
| `/legal/acceptable-use` | open sign-up |
| `/legal/refunds` (refund and cancellation policy) | Stripe |
| `/pricing` | Stripe ("what you are selling", "the purchase currency") |
| `/contact` (an email address, and the business's name and address) | Stripe (asks for contact details beyond a form, and a business address) |

## Terms of service: points to cover

- Who provides the service (the legal name and address the product owner
  gives Stripe), and that the service is offered as it is, with no uptime
  promise at this stage.
- The account: one person, signed in with Google or GitHub; one sign-up
  credit per person; the user is responsible for what their agents and
  API tokens do.
- A payment method is required to create or run anything. Saving a card
  charges nothing; the bank may show a temporary authorisation.
- What is sold: credit, in US dollars, that is used up by sessions: a rate
  per hour awake and a rate per GB-month of disk for as long as a session
  exists. Plans renew monthly until cancelled and give credit that expires
  at the period's end; purchased credit expires 12 months after purchase;
  the sign-up credit is one per person and per card and expires after 90
  days. Credit has no cash value, is not transferable, cannot be withdrawn
  and is not refunded except as the refund policy says.
- How use is counted (awake time includes the idle minutes before sleep;
  disk from creation to deletion), and that the meter's record decides.
- Sessions sleep when credit runs out or when the last payment method is
  removed, and are kept; after the stated number of days at a zero balance
  they are deleted; data is not backed up for the user.
- Auto-recharge, if the user turns it on: the user agrees that the service
  charges the saved card for the chosen pack whenever the balance falls
  below the chosen threshold, up to the chosen monthly cap, until they turn
  it off. (Stripe's guidance for charges made when the customer is not
  present asks for exactly this agreement, the timing, how the amount is
  determined, and a record of it.)
- Sessions run on interruptible machines and can be restarted at any time.
- Suspension and termination for breach of the acceptable-use policy, for
  chargebacks, or for risk to the service; what happens to data then.
- Changes to prices and terms: notice in the app, effective from the next
  period.
- Limitation of liability, governing law, contact. (Wording for a lawyer.)

## Privacy policy: points to cover

- What is collected: email address and name from Google or GitHub; the
  sessions and their content (disks, snapshots) which the user controls;
  usage records (which session was awake when, and what was charged); IP addresses in request
  logs and, hashed, at sign-up; payment details are collected and held by
  Stripe, not by the service (it keeps Stripe's customer, payment and
  payment-method IDs and the card's fingerprint, brand and last four digits).
- Why: to provide the service, to bill, to prevent abuse.
- Who else processes it: Google Cloud (hosting, us-west1), Stripe
  (payments), Google and GitHub (sign-in). No advertising, no sale of data.
- That the operator does not look inside sessions except to investigate
  abuse or at the user's request, and can technically access them.
- Retention: sessions until deleted (or the idle deletion); usage records
  13 months; logs for the period Google Cloud Logging keeps them; the record
  that a card has had the sign-up credit is kept without limit; Stripe keeps invoices as the law
  requires of it.
- Deleting the account (in the app), and how to ask for a copy or a
  correction (the contact address).
- Cookies: the sign-in cookie only.
- If users in the EU or UK are accepted: the lawful bases, transfers to
  the US, and the rights under GDPR. (For a lawyer; an option is to say
  the service is offered to US users only at first.)

## Acceptable use: points to cover

No: breaking the law; attacking, scanning or probing systems the user does
not own; sending spam or bulk messages; mining cryptocurrency; running
proxies, VPN exits, Tor relays or any service for third parties from a
session; evading another site's bans or creating accounts in bulk on other
services; content that is illegal where the user or the service is;
opening several accounts or using several cards to collect the sign-up
credit more than once; reselling without
agreement. Agents count as the user. Reports of abuse go to an address
(`abuse@`), and the service may suspend first and ask afterwards.

## Refund and cancellation policy: points to cover

- Cancel any time in "Manage billing"; the plan runs to the end of the
  paid period and does not renew.
- No refunds for part of a period or for unused credit, except where the
  law requires or the service was unavailable; requests go to the contact
  address and are decided case by case.
- A refunded pack's remaining credit is removed.
- A chargeback suspends the account until it is resolved.
