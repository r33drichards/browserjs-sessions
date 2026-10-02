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
- The account: one person, signed in with Google or GitHub; one free
  allowance per person; the user is responsible for what their agents and
  API tokens do.
- What is sold: hours of awake session time. Plans renew monthly until
  cancelled; a plan's hours expire at the period's end; purchased hours
  expire 12 months after purchase; hours have no cash value, are not
  transferable and are not refunded except as the refund policy says.
- How time is counted (awake time, including the idle minutes before
  sleep), and that the meter's record decides.
- Sessions sleep when hours run out and are kept; sessions of free and
  pay-as-you-go accounts unused for the stated number of days are deleted;
  data is not backed up for the user.
- Sessions run on interruptible machines and can be restarted at any time.
- Suspension and termination for breach of the acceptable-use policy, for
  chargebacks, or for risk to the service; what happens to data then.
- Changes to prices and terms: notice in the app, effective from the next
  period.
- Limitation of liability, governing law, contact. (Wording for a lawyer.)

## Privacy policy: points to cover

- What is collected: email address and name from Google or GitHub; the
  sessions and their content (disks, snapshots) which the user controls;
  usage records (which session was awake when); IP addresses in request
  logs and, hashed, at sign-up; payment details are collected and held by
  Stripe, not by the service (it keeps Stripe's customer and payment IDs).
- Why: to provide the service, to bill, to prevent abuse.
- Who else processes it: Google Cloud (hosting, us-west1), Stripe
  (payments), Google and GitHub (sign-in). No advertising, no sale of data.
- That the operator does not look inside sessions except to investigate
  abuse or at the user's request, and can technically access them.
- Retention: sessions until deleted (or the idle deletion); usage records
  13 months; logs for the period Google Cloud Logging keeps them; a deleted
  account's hashed marker 35 days; Stripe keeps invoices as the law
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
sharing an account to multiply the free allowance; reselling without
agreement. Agents count as the user. Reports of abuse go to an address
(`abuse@`), and the service may suspend first and ask afterwards.

## Refund and cancellation policy: points to cover

- Cancel any time in "Manage billing"; the plan runs to the end of the
  paid period and does not renew.
- No refunds for part of a period or for unused hours, except where the
  law requires or the service was unavailable; requests go to the contact
  address and are decided case by case.
- A refunded pack's remaining hours are removed.
- A chargeback suspends the account until it is resolved.
