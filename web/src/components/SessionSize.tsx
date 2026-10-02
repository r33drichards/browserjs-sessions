// A session's size on its page: what it runs at, the size waiting for its
// next start, and the way to change it.
import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import Modal from "@cloudscape-design/components/modal"
import RadioGroup from "@cloudscape-design/components/radio-group"
import SpaceBetween from "@cloudscape-design/components/space-between"
import { useEffect, useState } from "react"
import type { Session } from "../api"
import { signedOutHandled } from "../auth/signedOut"
import { useCreateGate } from "../billing/SessionBilling"
import { api } from "../shell"
import type { Sizes } from "../sizes"
import { resizeConsequence, sizeLabel, sizeNumbers } from "../sizes"

type Sized = Pick<Session, "id" | "name" | "state" | "size" | "pendingSize">

// "Small", or "Small, Large from its next start".
export function sizeLine(s: Pick<Session, "size" | "pendingSize">): string {
  if (!s.size) return ""
  return s.pendingSize ? `${sizeLabel(s.size)}, ${sizeLabel(s.pendingSize)} from its next start` : sizeLabel(s.size)
}

// `run` performs the request and reports failure itself (the page's own);
// it resolves to whether it succeeded.
export function SessionSize({ session, run }: { session: Sized; run: (fn: () => Promise<unknown>) => Promise<boolean> }) {
  const [sizes, setSizes] = useState<Sizes | null>(null)
  const [open, setOpen] = useState(false)
  const [choice, setChoice] = useState("")
  const [busy, setBusy] = useState(false)
  const gate = useCreateGate()

  useEffect(() => {
    let cancelled = false
    api
      .listSizes()
      .then(answer => {
        if (!cancelled && answer.sizes.length > 1) setSizes(answer)
      })
      .catch(signedOutHandled)
    return () => {
      cancelled = true
    }
  }, [])

  if (!session.size) return null
  const target = session.pendingSize ?? session.size

  async function save() {
    setBusy(true)
    const ok = await run(() => api.resizeSession(session.id, choice))
    setBusy(false)
    if (ok) setOpen(false)
  }

  return (
    <span data-testid="session-size">
      size: {sizeLine(session)}
      {sizes && (
        <>
          {" "}
          <Button
            variant="inline-link"
            onClick={() => {
              setChoice(target)
              setOpen(true)
            }}
          >
            change size
          </Button>
        </>
      )}
      {open && sizes && (
        <Modal
          visible
          onDismiss={() => setOpen(false)}
          header={`Change the size of ${session.name}`}
          footer={
            <Box float="right">
              <SpaceBetween direction="horizontal" size="xs">
                <Button variant="link" onClick={() => setOpen(false)}>
                  Cancel
                </Button>
                <Button variant="primary" loading={busy} disabled={choice === target} onClick={save}>
                  Change size
                </Button>
              </SpaceBetween>
            </Box>
          }
        >
          <SpaceBetween size="m">
            <RadioGroup
              ariaLabel="Size"
              value={choice}
              onChange={e => setChoice(e.detail.value)}
              items={sizes.sizes.map(s => {
                const hourly = gate.hourly(s.name)
                const included = gate.includes(s.name)
                return {
                  value: s.name,
                  label: sizeLabel(s.name),
                  disabled: !included,
                  description: [`${sizeNumbers(s)}.`, hourly ? `${hourly} an hour while awake.` : "", included ? "" : "Not included in your plan."]
                    .filter(Boolean)
                    .join(" "),
                }
              })}
            />
            <p data-testid="resize-consequence">{resizeConsequence(session.state)}</p>
          </SpaceBetween>
        </Modal>
      )}
    </span>
  )
}
