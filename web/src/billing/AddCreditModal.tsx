import Alert from "@cloudscape-design/components/alert"
import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import Modal from "@cloudscape-design/components/modal"
import SpaceBetween from "@cloudscape-design/components/space-between"
import Tiles from "@cloudscape-design/components/tiles"
import { useState } from "react"
import type { Catalogue } from "../billingApi"
import { credit, price } from "../billingApi"

interface Props {
  catalogue: Catalogue | null
  testMode: boolean
  busy: boolean
  onDismiss: () => void
  onBuy: (lookupKey: string) => void
}

// The packs as tiles; paying happens on Stripe's page.
export function AddCreditModal({ catalogue, testMode, busy, onDismiss, onBuy }: Props) {
  const packs = catalogue?.packs ?? []
  // The middle pack to start with: neither the least nor the most.
  const [chosen, setChosen] = useState(() => packs[Math.floor((packs.length - 1) / 2)]?.lookupKey ?? "")
  const validDays = packs.find(p => p.lookupKey === chosen)?.validDays
  const months = validDays ? Math.round(validDays / 30.4) : undefined

  return (
    <Modal
      visible
      onDismiss={onDismiss}
      header="Add credit"
      footer={
        <Box float="right">
          <SpaceBetween direction="horizontal" size="xs">
            <Button variant="link" onClick={onDismiss}>
              Cancel
            </Button>
            <Button variant="primary" loading={busy} disabled={!chosen} onClick={() => onBuy(chosen)}>
              Continue to payment
            </Button>
          </SpaceBetween>
        </Box>
      }
    >
      <SpaceBetween size="m">
        {packs.length === 0 ? (
          <Box>There is no credit for sale right now.</Box>
        ) : (
          <Tiles
            ariaLabel="Amount of credit"
            value={chosen}
            onChange={e => setChosen(e.detail.value)}
            columns={Math.min(packs.length, 3)}
            items={packs.map(p => ({
              value: p.lookupKey,
              label: credit(p.creditMicros),
              description: p.amount === Math.floor(p.creditMicros / 10_000) ? undefined : `for ${price(p.amount)}`,
            }))}
          />
        )}
        <Box>
          Credit is used after your plan&apos;s credit{months ? ` and is valid for ${months} months` : ""}.
        </Box>
        {testMode && (
          <Alert type="info">Test mode: no real money is taken. Use card 4242 4242 4242 4242.</Alert>
        )}
      </SpaceBetween>
    </Modal>
  )
}
