// The first-run gate: shown in place of every page to a signed-in user who
// has not accepted the terms, or has no card and no sessions yet.
import Alert from "@cloudscape-design/components/alert"
import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import Checkbox from "@cloudscape-design/components/checkbox"
import Container from "@cloudscape-design/components/container"
import ExpandableSection from "@cloudscape-design/components/expandable-section"
import Header from "@cloudscape-design/components/header"
import SpaceBetween from "@cloudscape-design/components/space-between"
import { useState } from "react"
import { signedOutHandled } from "../auth/signedOut"
import { useBilling } from "../billing/BillingProvider"
import { PlanList } from "../billing/PlanList"
import { siteUrl } from "../billing/site"
import { aboutHours, awakeHours, billingApi, credit, ratesInWords } from "../billingApi"
import { Shell } from "../shell"

export function Welcome({ step }: { step: "terms" | "card" }) {
  const { billing, catalogue, run, busy, reload } = useBilling()
  const [accepted, setAccepted] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState("")
  if (!billing) return null

  const words = ratesInWords(billing.rates)
  const signup = catalogue?.signupCredit
  const version = billing.termsRequired

  async function accept() {
    if (!version) return
    setSaving(true)
    setError("")
    try {
      await billingApi.acceptTerms(version)
      reload()
    } catch (e) {
      if (!signedOutHandled(e)) setError(String(e instanceof Error ? e.message : e))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Shell>
      <SpaceBetween size="l">
        <Header variant="h1">Welcome</Header>

        {version && (
          <Container header={<Header variant="h2">1. Terms</Header>}>
            <SpaceBetween size="s">
              <Box>
                Read the <a href={siteUrl("/legal/terms")}>terms of service</a>, the{" "}
                <a href={siteUrl("/legal/privacy")}>privacy policy</a> and the{" "}
                <a href={siteUrl("/legal/acceptable-use")}>acceptable-use policy</a>.
              </Box>
              <Checkbox checked={accepted} onChange={e => setAccepted(e.detail.checked)}>
                I accept the terms of service and the acceptable-use policy.
              </Checkbox>
              {error ? <Alert type="error">{error}</Alert> : null}
              <Button variant="primary" disabled={!accepted} loading={saving} onClick={accept}>
                Continue
              </Button>
            </SpaceBetween>
          </Container>
        )}

        <Container header={<Header variant="h2">{version ? "2. " : ""}Add a payment method</Header>}>
          <SpaceBetween size="s">
            <Box>
              A card is required before you create a desktop. Nothing is charged now; your bank may show a temporary
              authorisation.
            </Box>
            {signup?.amountMicros ? (
              <Box>
                When your card is saved you get <b>{credit(signup.amountMicros)} of credit</b>, enough for{" "}
                {aboutHours(awakeHours(signup.amountMicros, billing.rates))} awake
                {signup.validDays ? `, valid for ${signup.validDays} days` : ""}. One credit per person and per card.
              </Box>
            ) : null}
            <Box>
              {words.awake} for each hour a session is awake · {words.kept} a month for each session you keep.{" "}
              <a href={siteUrl("/pricing")}>Pricing</a>
            </Box>
            <ExpandableSection headerText="See plans">
              <PlanList />
            </ExpandableSection>
            {billing.payments === "test" && (
              <Alert type="info">Test mode: no real money is taken. Use card 4242 4242 4242 4242.</Alert>
            )}
            <Box float="right">
              <Button
                variant="primary"
                disabled={step === "terms"}
                disabledReason="Accept the terms first."
                loading={busy === "add-card"}
                onClick={() => run("add-card")}
              >
                Add a card
              </Button>
            </Box>
          </SpaceBetween>
        </Container>
      </SpaceBetween>
    </Shell>
  )
}
